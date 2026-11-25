package storage

import (
	"log/slog"
	"sync/atomic"
)

// Health tracks whether the object store is reachable. A nil *Health is always healthy.
type Health struct {
	down atomic.Bool
}

func (h *Health) Healthy() bool { return h == nil || !h.down.Load() }

// set records a call or probe outcome: nil marks the store healthy, anything else unhealthy.
func (h *Health) set(err error) {
	if h == nil {
		return
	}
	if was := h.down.Swap(err != nil); was != (err != nil) {
		if err != nil {
			slog.Warn("object store unhealthy", "error", err)
		} else {
			slog.Info("object store healthy")
		}
	}
}
