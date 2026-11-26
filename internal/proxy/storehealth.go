package proxy

import (
	"context"
	"log/slog"
	"time"

	"github.com/IUCLabs/artifactapi/internal/cache"
	"github.com/IUCLabs/artifactapi/pkg/models"
)

// ProbeConfig tunes the object store health probe.
type ProbeConfig struct {
	UpThreshold   int
	DownThreshold int
	Interval      time.Duration
	Timeout       time.Duration
}

// Apply overlays the set fields of s onto c.
func (c ProbeConfig) Apply(s models.Settings) ProbeConfig {
	if s.S3HealthUpThreshold != nil {
		c.UpThreshold = *s.S3HealthUpThreshold
	}
	if s.S3HealthDownThreshold != nil {
		c.DownThreshold = *s.S3HealthDownThreshold
	}
	if s.S3HealthIntervalMs != nil {
		c.Interval = time.Duration(*s.S3HealthIntervalMs) * time.Millisecond
	}
	if s.S3HealthTimeoutMs != nil {
		c.Timeout = time.Duration(*s.S3HealthTimeoutMs) * time.Millisecond
	}
	return c
}

// Settings expresses c with every field set.
func (c ProbeConfig) Settings() models.Settings {
	up, down := c.UpThreshold, c.DownThreshold
	interval, timeout := int(c.Interval.Milliseconds()), int(c.Timeout.Milliseconds())
	return models.Settings{S3HealthUpThreshold: &up, S3HealthDownThreshold: &down, S3HealthIntervalMs: &interval, S3HealthTimeoutMs: &timeout}
}

func (c ProbeConfig) Validate() error {
	return c.Settings().Validate()
}

// settingsLoadTimeout bounds each settings read independently of the probe timeout it may change.
const settingsLoadTimeout = 2 * time.Second

// SettingsLoader reads the stored settings overrides.
type SettingsLoader func(context.Context) (models.Settings, error)

// probeSettings resolves the effective config, keeping the last good one on failure.
type probeSettings struct {
	defaults ProbeConfig
	load     SettingsLoader
	failing  bool
}

func (p *probeSettings) refresh(ctx context.Context, current ProbeConfig) ProbeConfig {
	if p.load == nil {
		return current
	}
	lctx, cancel := context.WithTimeout(ctx, settingsLoadTimeout)
	s, err := p.load(lctx)
	cancel()
	if err == nil {
		next := p.defaults.Apply(s)
		if err = next.Validate(); err == nil {
			if p.failing {
				slog.Info("object store probe settings readable again")
			}
			p.failing = false
			return next
		}
	}
	if !p.failing && ctx.Err() == nil {
		slog.Warn("object store probe settings not applied, keeping last known", "error", err)
	}
	p.failing = true
	return current
}

// hysteresis settles on up or down only after a threshold of consecutive like results.
type hysteresis struct {
	cfg       ProbeConfig
	oks, errs int
	known, up bool
}

func (h *hysteresis) observe(err error) {
	if err == nil {
		h.oks, h.errs = h.oks+1, 0
		if h.oks >= h.cfg.UpThreshold {
			h.known, h.up = true, true
		}
		return
	}
	h.oks, h.errs = 0, h.errs+1
	if h.errs >= h.cfg.DownThreshold {
		h.known, h.up = true, false
	}
}

// healthTTL outlives the longest gap between writes: the first probe can wait out a stalled
// settings read, a fixed-rate tick can slip by up to an interval, then a hung probe.
func healthTTL(cfg ProbeConfig) time.Duration {
	return 3*cfg.Interval + settingsLoadTimeout + cfg.Timeout
}

func logProbeSettings(c ProbeConfig) {
	slog.Info("object store probe settings changed", "up_threshold", c.UpThreshold, "down_threshold", c.DownThreshold, "interval", c.Interval, "timeout", c.Timeout)
}

// settingsRefreshInterval is how often settings are re-read, independent of the probe interval.
const settingsRefreshInterval = time.Second

// watchSettings re-reads settings every settingsRefreshInterval and publishes each change to out,
// replacing any value not yet taken.
func watchSettings(ctx context.Context, settings *probeSettings, cfg ProbeConfig, out chan ProbeConfig) {
	ticker := time.NewTicker(settingsRefreshInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		next := settings.refresh(ctx, cfg)
		if next == cfg {
			continue
		}
		logProbeSettings(next)
		cfg = next
		select {
		case <-out:
		default:
		}
		out <- next
	}
}

// RunStoreProbe probes the store every interval and writes this replica's settled state to
// Redis with a healthTTL expiry. Nothing is written until a threshold is first crossed.
// The config is defaults overlaid with load's settings, re-read every settingsRefreshInterval;
// an interval change reschedules the next probe at once. load may be nil.
func RunStoreProbe(ctx context.Context, defaults ProbeConfig, load SettingsLoader, probe func(context.Context) error, c *cache.Redis) {
	settings := &probeSettings{defaults: defaults, load: load}
	cfg := settings.refresh(ctx, defaults)
	if cfg != defaults {
		logProbeSettings(cfg)
	}
	h := hysteresis{cfg: cfg}
	updates := make(chan ProbeConfig, 1)
	if load != nil {
		wctx, stop := context.WithCancel(ctx)
		watched := make(chan struct{})
		go func() {
			watchSettings(wctx, settings, cfg, updates)
			close(watched)
		}()
		defer func() { stop(); <-watched }()
	}
	apply := func(next ProbeConfig) {
		cfg, h.cfg = next, next
	}
	write := func() {
		if !h.known {
			return
		}
		if werr := c.SetStoreHealth(ctx, h.up, healthTTL(cfg)); werr != nil {
			slog.Warn("write store health failed", "error", werr)
		}
	}
	timer := time.NewTimer(0)
	defer timer.Stop()
	var last time.Time
	for {
		select {
		case <-ctx.Done():
			return
		case next := <-updates:
			changed := next.Interval != cfg.Interval
			apply(next)
			if changed && !last.IsZero() {
				timer.Reset(max(0, cfg.Interval-time.Since(last)))
				// The written TTL was sized for the old interval.
				write()
			}
			continue
		case <-timer.C:
		}
		select {
		case next := <-updates:
			apply(next)
		default:
		}
		last = time.Now()
		pctx, cancel := context.WithTimeout(ctx, cfg.Timeout)
		err := probe(pctx)
		cancel()
		if ctx.Err() != nil {
			return
		}
		known, up := h.known, h.up
		h.observe(err)
		if h.known && (!known || up != h.up) {
			slog.Info("object store health settled", "up", h.up, "error", err)
		}
		write()
		timer.Reset(max(0, cfg.Interval-time.Since(last)))
	}
}
