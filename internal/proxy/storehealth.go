package proxy

import (
	"context"
	"errors"
	"fmt"
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
	switch {
	case c.UpThreshold < 1 || c.DownThreshold < 1:
		return errors.New("s3 health thresholds must be at least 1")
	case c.Interval <= 0 || c.Timeout <= 0:
		return errors.New("s3 health interval and timeout must be positive")
	case c.Timeout >= c.Interval:
		return fmt.Errorf("s3 health timeout %s must be shorter than interval %s", c.Timeout, c.Interval)
	}
	return nil
}

// SettingsLoader reads the stored settings overrides.
type SettingsLoader func(context.Context) (models.Settings, error)

// probeSettings resolves the effective config each tick, keeping the last good one on failure.
type probeSettings struct {
	defaults ProbeConfig
	load     SettingsLoader
	failing  bool
}

func (p *probeSettings) refresh(ctx context.Context, current ProbeConfig) ProbeConfig {
	if p.load == nil {
		return current
	}
	lctx, cancel := context.WithTimeout(ctx, current.Timeout)
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

// RunStoreProbe probes the store every interval and writes this replica's settled state to
// Redis with a TTL of three intervals. Nothing is written until a threshold is first crossed.
// Each tick re-resolves the config as defaults overlaid with load's settings; load may be nil.
func RunStoreProbe(ctx context.Context, defaults ProbeConfig, load SettingsLoader, probe func(context.Context) error, c *cache.Redis) {
	settings := probeSettings{defaults: defaults, load: load}
	cfg := defaults
	h := hysteresis{cfg: cfg}
	ticker := time.NewTicker(cfg.Interval)
	defer ticker.Stop()
	for {
		if next := settings.refresh(ctx, cfg); next != cfg {
			slog.Info("object store probe settings changed", "up_threshold", next.UpThreshold, "down_threshold", next.DownThreshold, "interval", next.Interval, "timeout", next.Timeout)
			if next.Interval != cfg.Interval {
				ticker.Reset(next.Interval)
			}
			cfg, h.cfg = next, next
		}
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
		if h.known {
			if werr := c.SetStoreHealth(ctx, h.up, 3*cfg.Interval); werr != nil {
				slog.Warn("write store health failed", "error", werr)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
