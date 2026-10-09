package database

import (
	"context"
	"time"

	"github.com/IUCLabs/artifactapi/internal/provider"
	"github.com/IUCLabs/artifactapi/pkg/models"
)

// ListGitHubAlpineRemotes returns every github_alpine remote so the syncer can
// sweep them on each poll tick.
func (db *DB) ListGitHubAlpineRemotes(ctx context.Context) ([]models.Remote, error) {
	rows, err := db.Pool.Query(ctx, `SELECT `+remoteCols+` FROM remotes WHERE package_type = $1 ORDER BY name`, models.PackageGitHubAlpine)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var remotes []models.Remote
	for rows.Next() {
		var r models.Remote
		if err := scanRemote(rows, &r); err != nil {
			return nil, err
		}
		remotes = append(remotes, r)
	}
	return remotes, rows.Err()
}

// ClaimGitHubAlpineSyncLease atomically claims the per-remote github_alpine sync
// lease. See claimSyncLease.
func (db *DB) ClaimGitHubAlpineSyncLease(ctx context.Context, remoteName, owner string, freshness, lease time.Duration) (bool, string, error) {
	return db.claimSyncLease(ctx, "github_alpine_sync_state", remoteName, owner, freshness, lease)
}

// ReleaseGitHubAlpineSyncLease records a github_alpine scan outcome and frees the
// lease. See releaseSyncLease.
func (db *DB) ReleaseGitHubAlpineSyncLease(ctx context.Context, remoteName, owner string, res provider.SyncResult) error {
	return db.releaseSyncLease(ctx, "github_alpine_sync_state", remoteName, owner, res)
}
