package gc

import (
	"context"
	"log/slog"
	"time"

	"github.com/IUCLabs/artifactapi/internal/database"
	"github.com/IUCLabs/artifactapi/internal/storage"
)

// blobGracePeriod is how old an orphaned blob must be before GC will delete
// it. This avoids racing in-flight dedup uploads that insert the blob row
// before the referencing artifact/local_files row exists.
const blobGracePeriod = 1 * time.Hour

type Collector struct {
	db       *database.DB
	store    *storage.S3
	interval time.Duration
}

func New(db *database.DB, store *storage.S3, interval time.Duration) *Collector {
	return &Collector{db: db, store: store, interval: interval}
}

func (c *Collector) Run(ctx context.Context) {
	slog.Info("gc started", "interval", c.interval)
	ticker := time.NewTicker(c.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			slog.Info("gc stopped")
			return
		case <-ticker.C:
			c.sweep(ctx)
		}
	}
}

func (c *Collector) sweep(ctx context.Context) {
	start := time.Now()

	orphaned, err := c.db.FindOrphanedBlobs(ctx, blobGracePeriod)
	if err != nil {
		slog.Error("gc: find orphaned blobs", "error", err)
		return
	}

	deleted := 0
	for _, blob := range orphaned {
		if err := c.store.Delete(ctx, blob.S3Key); err != nil {
			slog.Warn("gc: delete s3 object", "key", blob.S3Key, "error", err)
			continue
		}
		if err := c.db.DeleteBlob(ctx, blob.ContentHash); err != nil {
			slog.Warn("gc: delete blob row", "hash", blob.ContentHash, "error", err)
			continue
		}
		deleted++
	}

	if deleted > 0 || len(orphaned) > 0 {
		slog.Info("gc sweep complete",
			"orphaned_found", len(orphaned),
			"deleted", deleted,
			"duration_ms", time.Since(start).Milliseconds(),
		)
	}
}
