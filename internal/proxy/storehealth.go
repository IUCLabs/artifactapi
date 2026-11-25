package proxy

import (
	"context"
	"log/slog"
	"time"

	"github.com/IUCLabs/artifactapi/internal/cache"
)

// ProbeConfig tunes the object store health probe.
type ProbeConfig struct {
	UpThreshold   int
	DownThreshold int
	Interval      time.Duration
	Timeout       time.Duration
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

// RunStoreProbe probes the store every cfg.Interval and writes this replica's settled state to
// Redis with a TTL of three intervals. Nothing is written until a threshold is first crossed.
func RunStoreProbe(ctx context.Context, cfg ProbeConfig, probe func(context.Context) error, c *cache.Redis) {
	h := hysteresis{cfg: cfg}
	ticker := time.NewTicker(cfg.Interval)
	defer ticker.Stop()
	for {
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
