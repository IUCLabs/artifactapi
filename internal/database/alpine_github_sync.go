package database

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

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

// ClaimGitHubAlpineSyncLease atomically claims the per-remote sync lease. It
// succeeds only when the remote is due (never synced, or synced longer than
// freshness ago) and no live lease is held by another replica. A zero freshness
// (prime scans) ignores the recency gate. The returned etag is the stored
// releases-list ETag, shared across replicas.
func (db *DB) ClaimGitHubAlpineSyncLease(ctx context.Context, remoteName, owner string, freshness, lease time.Duration) (bool, string, error) {
	row := db.Pool.QueryRow(ctx, `
		INSERT INTO github_alpine_sync_state AS s (remote_name, sync_lease_owner, sync_lease_expires)
		VALUES ($1, $2, now() + make_interval(secs => $4))
		ON CONFLICT (remote_name) DO UPDATE
			SET sync_lease_owner   = $2,
			    sync_lease_expires = now() + make_interval(secs => $4)
			WHERE (s.last_synced_at IS NULL OR s.last_synced_at < now() - make_interval(secs => $3))
			  AND (s.sync_lease_expires IS NULL OR s.sync_lease_expires < now())
		RETURNING s.etag
	`, remoteName, owner, freshness.Seconds(), lease.Seconds())

	var etag string
	if err := row.Scan(&etag); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return false, "", nil
		}
		return false, "", err
	}
	return true, etag, nil
}

// ReleaseGitHubAlpineSyncLease records the completed scan and frees the lease.
// Only the owning replica may release; last_synced_at advances so the next poll
// waits a full freshness window, and etag is persisted for the next conditional
// request.
func (db *DB) ReleaseGitHubAlpineSyncLease(ctx context.Context, remoteName, owner, etag string, syncedAt time.Time) error {
	_, err := db.Pool.Exec(ctx, `
		UPDATE github_alpine_sync_state
		   SET last_synced_at = $3, etag = $4, sync_lease_owner = '', sync_lease_expires = NULL
		 WHERE remote_name = $1 AND sync_lease_owner = $2
	`, remoteName, owner, syncedAt, etag)
	return err
}
