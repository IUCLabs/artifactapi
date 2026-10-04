package database

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/IUCLabs/artifactapi/pkg/models"
)

func (db *DB) UpsertBlob(ctx context.Context, contentHash, s3Key string, sizeBytes int64, contentType string) error {
	_, err := db.Pool.Exec(ctx, `
		INSERT INTO blobs (content_hash, s3_key, size_bytes, content_type)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (content_hash) DO NOTHING
	`, contentHash, s3Key, sizeBytes, contentType)
	return err
}

func (db *DB) UpsertArtifact(ctx context.Context, remoteName, path, contentHash, upstreamETag string) error {
	_, err := db.Pool.Exec(ctx, `
		INSERT INTO artifacts (remote_name, path, content_hash, upstream_etag)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (remote_name, path) DO UPDATE SET
			content_hash = EXCLUDED.content_hash,
			upstream_etag = EXCLUDED.upstream_etag,
			last_fetched_at = NOW(),
			fetch_count = artifacts.fetch_count + 1
	`, remoteName, path, contentHash, upstreamETag)
	return err
}

func (db *DB) GetArtifact(ctx context.Context, remoteName, path string) (*models.Artifact, error) {
	row := db.Pool.QueryRow(ctx, `
		SELECT a.id, a.remote_name, a.path, a.content_hash, a.upstream_etag,
			a.upstream_last_modified, a.first_seen_at, a.last_fetched_at,
			a.last_accessed_at, a.fetch_count, a.access_count,
			b.size_bytes, b.content_type
		FROM artifacts a
		JOIN blobs b ON a.content_hash = b.content_hash
		WHERE a.remote_name = $1 AND a.path = $2
	`, remoteName, path)

	var a models.Artifact
	err := row.Scan(
		&a.ID, &a.RemoteName, &a.Path, &a.ContentHash, &a.UpstreamETag,
		&a.UpstreamLastModified, &a.FirstSeenAt, &a.LastFetchedAt,
		&a.LastAccessedAt, &a.FetchCount, &a.AccessCount,
		&a.SizeBytes, &a.ContentType,
	)
	if err != nil {
		return nil, err
	}
	return &a, nil
}

func (db *DB) TouchArtifactAccess(ctx context.Context, remoteName, path string) error {
	_, err := db.Pool.Exec(ctx, `
		UPDATE artifacts SET
			last_accessed_at = NOW(),
			access_count = access_count + 1
		WHERE remote_name = $1 AND path = $2
	`, remoteName, path)
	return err
}

// ListArtifacts pages a remote's artifacts whose path starts with prefix ("" lists all).
func (db *DB) ListArtifacts(ctx context.Context, remoteName, prefix string, limit, offset int) ([]models.Artifact, error) {
	rows, err := db.Pool.Query(ctx, `
		SELECT a.id, a.remote_name, a.path, a.content_hash, a.upstream_etag,
			a.upstream_last_modified, a.first_seen_at, a.last_fetched_at,
			a.last_accessed_at, a.fetch_count, a.access_count,
			b.size_bytes, b.content_type
		FROM artifacts a
		JOIN blobs b ON a.content_hash = b.content_hash
		WHERE a.remote_name = $1 AND left(a.path, length($2)) = $2
		ORDER BY a.path
		LIMIT $3 OFFSET $4
	`, remoteName, prefix, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var artifacts []models.Artifact
	for rows.Next() {
		var a models.Artifact
		if err := rows.Scan(
			&a.ID, &a.RemoteName, &a.Path, &a.ContentHash, &a.UpstreamETag,
			&a.UpstreamLastModified, &a.FirstSeenAt, &a.LastFetchedAt,
			&a.LastAccessedAt, &a.FetchCount, &a.AccessCount,
			&a.SizeBytes, &a.ContentType,
		); err != nil {
			return nil, err
		}
		artifacts = append(artifacts, a)
	}
	return artifacts, rows.Err()
}

func (db *DB) DeleteArtifact(ctx context.Context, remoteName, path string) error {
	_, err := db.Pool.Exec(ctx, `DELETE FROM artifacts WHERE remote_name = $1 AND path = $2`, remoteName, path)
	return err
}

// DeleteArtifactsByPrefix removes every artifact row of a remote whose path
// starts with prefix.
func (db *DB) DeleteArtifactsByPrefix(ctx context.Context, remoteName, prefix string) error {
	_, err := db.Pool.Exec(ctx, `DELETE FROM artifacts WHERE remote_name = $1 AND left(path, length($2)) = $2`, remoteName, prefix)
	return err
}

func (db *DB) InsertAccessLog(ctx context.Context, remoteName, path string, cacheHit bool, sizeBytes int64, upstreamMS int, clientIP string) error {
	_, err := db.Pool.Exec(ctx, `
		INSERT INTO access_log (remote_name, path, cache_hit, size_bytes, upstream_ms, client_ip)
		VALUES ($1, $2, $3, $4, $5, $6)
	`, remoteName, path, cacheHit, sizeBytes, upstreamMS, clientIP)
	return err
}

// AccessLogEntry is one buffered access-log record.
type AccessLogEntry struct {
	RemoteName string
	Path       string
	CacheHit   bool
	SizeBytes  int64
	UpstreamMS int
	ClientIP   string
}

// InsertAccessLogBatch bulk-inserts access-log rows with a single COPY.
func (db *DB) InsertAccessLogBatch(ctx context.Context, entries []AccessLogEntry) error {
	if len(entries) == 0 {
		return nil
	}
	rows := make([][]any, len(entries))
	for i, e := range entries {
		rows[i] = []any{e.RemoteName, e.Path, e.CacheHit, e.SizeBytes, e.UpstreamMS, e.ClientIP}
	}
	_, err := db.Pool.CopyFrom(ctx,
		pgx.Identifier{"access_log"},
		[]string{"remote_name", "path", "cache_hit", "size_bytes", "upstream_ms", "client_ip"},
		pgx.CopyFromRows(rows),
	)
	return err
}

// FindOrphanedBlobs returns blobs no longer referenced by any artifact or
// local file, restricted to those created before now()-minAge. The age cutoff
// is a grace period that avoids a TOCTOU race with in-flight dedup uploads,
// which insert the blob row before the referencing artifact/local_files row.
func (db *DB) FindOrphanedBlobs(ctx context.Context, minAge time.Duration) ([]models.Blob, error) {
	cutoff := time.Now().Add(-minAge)
	rows, err := db.Pool.Query(ctx, `
		SELECT b.content_hash, b.s3_key, b.size_bytes, b.content_type, b.created_at
		FROM blobs b
		WHERE b.created_at < $1
		  AND b.content_hash NOT IN (
			SELECT content_hash FROM artifacts
			UNION
			SELECT content_hash FROM local_files
		)
	`, cutoff)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var blobs []models.Blob
	for rows.Next() {
		var b models.Blob
		if err := rows.Scan(&b.ContentHash, &b.S3Key, &b.SizeBytes, &b.ContentType, &b.CreatedAt); err != nil {
			return nil, err
		}
		blobs = append(blobs, b)
	}
	return blobs, rows.Err()
}

func (db *DB) DeleteBlob(ctx context.Context, contentHash string) error {
	_, err := db.Pool.Exec(ctx, `DELETE FROM blobs WHERE content_hash = $1`, contentHash)
	return err
}

func (db *DB) DeleteColdArtifacts(ctx context.Context, remoteName string, olderThan time.Duration) (int64, error) {
	cutoff := time.Now().Add(-olderThan)
	tag, err := db.Pool.Exec(ctx, `
		DELETE FROM artifacts
		WHERE remote_name = $1 AND last_accessed_at < $2
	`, remoteName, cutoff)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}
