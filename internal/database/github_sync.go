package database

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/IUCLabs/artifactapi/internal/provider"

	"github.com/IUCLabs/artifactapi/pkg/models"
)

// ListGitHubRPMRemotes returns every github_rpm remote so the syncer can sweep
// them on each poll tick.
func (db *DB) ListGitHubRPMRemotes(ctx context.Context) ([]models.Remote, error) {
	rows, err := db.Pool.Query(ctx, `SELECT `+remoteCols+` FROM remotes WHERE package_type = $1 ORDER BY name`, models.PackageGitHubRPM)
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

// ClaimGitHubSyncLease atomically claims the per-remote github_rpm sync lease.
// See claimSyncLease.
func (db *DB) ClaimGitHubSyncLease(ctx context.Context, remoteName, owner string, freshness, lease time.Duration) (bool, string, error) {
	return db.claimSyncLease(ctx, "github_rpm_sync_state", remoteName, owner, freshness, lease)
}

// ReleaseGitHubSyncLease records a github_rpm scan outcome and frees the lease.
// See releaseSyncLease.
func (db *DB) ReleaseGitHubSyncLease(ctx context.Context, remoteName, owner string, res provider.SyncResult) error {
	return db.releaseSyncLease(ctx, "github_rpm_sync_state", remoteName, owner, res)
}

// claimSyncLease atomically claims a per-remote sync lease in table. It succeeds
// (claimed=true) only when the remote is due and no live lease is held by
// another replica. Due means: a pending retry after a failed scan has reached
// next_retry_at, or, with no retry pending, the remote was never synced or was
// synced longer than freshness ago. A zero freshness (prime scans) ignores the
// recency gate but still honours a pending retry's backoff. The returned etag is
// the stored releases-list ETag, shared across replicas so a conditional request
// can short-circuit an unchanged repo.
func (db *DB) claimSyncLease(ctx context.Context, table, remoteName, owner string, freshness, lease time.Duration) (bool, string, error) {
	row := db.Pool.QueryRow(ctx, `
		INSERT INTO `+table+` AS s (remote_name, sync_lease_owner, sync_lease_expires)
		VALUES ($1, $2, now() + make_interval(secs => $4))
		ON CONFLICT (remote_name) DO UPDATE
			SET sync_lease_owner   = $2,
			    sync_lease_expires = now() + make_interval(secs => $4)
			WHERE (CASE WHEN s.next_retry_at IS NOT NULL THEN s.next_retry_at <= now()
			            ELSE s.last_synced_at IS NULL OR s.last_synced_at < now() - make_interval(secs => $3) END)
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

// releaseSyncLease records a scan outcome and frees the lease; only the owning
// replica may release. Success advances last_synced_at, persists the etag and
// clears any retry. Failure leaves last_synced_at and etag untouched (the last
// good metadata keeps serving) and schedules next_retry_at with exponential
// backoff from the consecutive-failure count, never before res.RetryAt.
func (db *DB) releaseSyncLease(ctx context.Context, table, remoteName, owner string, res provider.SyncResult) error {
	var retryAt *time.Time
	if !res.RetryAt.IsZero() {
		retryAt = &res.RetryAt
	}
	_, err := db.Pool.Exec(ctx, `
		UPDATE `+table+`
		   SET last_synced_at = CASE WHEN $3 THEN last_synced_at ELSE now() END,
		       etag           = CASE WHEN $3 THEN etag ELSE $4 END,
		       sync_failures  = CASE WHEN $3 THEN sync_failures + 1 ELSE 0 END,
		       next_retry_at  = CASE WHEN $3 THEN GREATEST(
		                            now() + make_interval(secs => LEAST($5 * power(2, LEAST(sync_failures, 20)), $6)),
		                            $7::timestamptz)
		                        END,
		       sync_lease_owner = '', sync_lease_expires = NULL
		 WHERE remote_name = $1 AND sync_lease_owner = $2
	`, remoteName, owner, res.Failed, res.Etag, res.Backoff.Seconds(), res.MaxBackoff.Seconds(), retryAt)
	return err
}
