package database

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

type DB struct {
	Pool *pgxpool.Pool
}

func New(dsn string) (*DB, error) {
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		return nil, fmt.Errorf("connect to postgres: %w", err)
	}

	if err := pool.Ping(context.Background()); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping postgres: %w", err)
	}

	db := &DB{Pool: pool}
	if err := db.migrate(); err != nil {
		pool.Close()
		return nil, fmt.Errorf("run migrations: %w", err)
	}

	return db, nil
}

func (db *DB) Close() {
	db.Pool.Close()
}

func (db *DB) migrate() error {
	ctx := context.Background()

	_, err := db.Pool.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS remotes (
			name            TEXT PRIMARY KEY,
			package_type    TEXT NOT NULL,
			repo_type       TEXT DEFAULT 'remote',
			base_url        TEXT NOT NULL DEFAULT '',
			description     TEXT DEFAULT '',
			username        TEXT DEFAULT '',
			password        TEXT DEFAULT '',
			immutable_ttl   INTEGER DEFAULT 0,
			mutable_ttl     INTEGER DEFAULT 3600,
			check_mutable   BOOLEAN DEFAULT TRUE,
			patterns            TEXT[] DEFAULT '{}',
			blocklist           TEXT[] DEFAULT '{}',
			mutable_patterns    TEXT[] DEFAULT '{}',
			immutable_patterns  TEXT[] DEFAULT '{}',
			ban_tags_enabled    BOOLEAN DEFAULT FALSE,
			ban_tags            TEXT[] DEFAULT '{}',
			quarantine_enabled  BOOLEAN DEFAULT FALSE,
			quarantine_days     INTEGER DEFAULT 3,
			stale_on_error      BOOLEAN DEFAULT TRUE,
			releases_remote     TEXT DEFAULT '',
			managed_by          TEXT DEFAULT '',
			created_at      TIMESTAMPTZ DEFAULT NOW(),
			updated_at      TIMESTAMPTZ DEFAULT NOW()
		);

		CREATE TABLE IF NOT EXISTS virtuals (
			name            TEXT PRIMARY KEY,
			package_type    TEXT NOT NULL,
			description     TEXT DEFAULT '',
			members         TEXT[] NOT NULL,
			managed_by      TEXT DEFAULT '',
			created_at      TIMESTAMPTZ DEFAULT NOW(),
			updated_at      TIMESTAMPTZ DEFAULT NOW()
		);

		CREATE TABLE IF NOT EXISTS blobs (
			content_hash    TEXT PRIMARY KEY,
			s3_key          TEXT NOT NULL,
			size_bytes      BIGINT NOT NULL,
			content_type    TEXT DEFAULT 'application/octet-stream',
			created_at      TIMESTAMPTZ DEFAULT NOW()
		);

		CREATE TABLE IF NOT EXISTS artifacts (
			id              BIGSERIAL PRIMARY KEY,
			remote_name     TEXT NOT NULL REFERENCES remotes(name) ON DELETE CASCADE,
			path            TEXT NOT NULL,
			content_hash    TEXT NOT NULL REFERENCES blobs(content_hash),
			upstream_etag   TEXT DEFAULT '',
			upstream_last_modified TIMESTAMPTZ,
			first_seen_at   TIMESTAMPTZ DEFAULT NOW(),
			last_fetched_at TIMESTAMPTZ DEFAULT NOW(),
			last_accessed_at TIMESTAMPTZ DEFAULT NOW(),
			fetch_count     BIGINT DEFAULT 1,
			access_count    BIGINT DEFAULT 1,
			UNIQUE(remote_name, path)
		);

		CREATE INDEX IF NOT EXISTS idx_artifacts_remote ON artifacts(remote_name);
		CREATE INDEX IF NOT EXISTS idx_artifacts_last_accessed ON artifacts(last_accessed_at);

		CREATE TABLE IF NOT EXISTS local_files (
			id              BIGSERIAL PRIMARY KEY,
			repo_name       TEXT NOT NULL,
			file_path       TEXT NOT NULL,
			content_hash    TEXT NOT NULL REFERENCES blobs(content_hash),
			created_at      TIMESTAMPTZ DEFAULT NOW(),
			UNIQUE(repo_name, file_path)
		);

		CREATE TABLE IF NOT EXISTS access_log (
			id              BIGSERIAL PRIMARY KEY,
			remote_name     TEXT NOT NULL,
			path            TEXT NOT NULL,
			cache_hit       BOOLEAN NOT NULL,
			size_bytes      BIGINT DEFAULT 0,
			upstream_ms     INTEGER DEFAULT 0,
			client_ip       TEXT DEFAULT '',
			created_at      TIMESTAMPTZ DEFAULT NOW()
		);

		CREATE INDEX IF NOT EXISTS idx_access_log_remote_time ON access_log(remote_name, created_at);

		ALTER TABLE remotes ADD COLUMN IF NOT EXISTS repo_type TEXT DEFAULT 'remote';
		ALTER TABLE remotes ADD COLUMN IF NOT EXISTS upstream_dial_timeout INTEGER DEFAULT 0;
		ALTER TABLE remotes ADD COLUMN IF NOT EXISTS upstream_tls_timeout INTEGER DEFAULT 0;
		ALTER TABLE remotes ADD COLUMN IF NOT EXISTS upstream_response_header_timeout INTEGER DEFAULT 0;

		CREATE TABLE IF NOT EXISTS rpm_metadata (
			id              BIGSERIAL PRIMARY KEY,
			repo_name       TEXT NOT NULL,
			file_path       TEXT NOT NULL,
			content_hash    TEXT NOT NULL,
			name            TEXT NOT NULL,
			epoch           INTEGER DEFAULT 0,
			version         TEXT NOT NULL,
			release         TEXT NOT NULL,
			arch            TEXT NOT NULL,
			summary         TEXT DEFAULT '',
			description     TEXT DEFAULT '',
			rpm_size        BIGINT DEFAULT 0,
			installed_size  BIGINT DEFAULT 0,
			license         TEXT DEFAULT '',
			vendor          TEXT DEFAULT '',
			build_group     TEXT DEFAULT '',
			build_host      TEXT DEFAULT '',
			source_rpm      TEXT DEFAULT '',
			url             TEXT DEFAULT '',
			packager        TEXT DEFAULT '',
			requires        JSONB DEFAULT '[]',
			provides        JSONB DEFAULT '[]',
			conflicts       JSONB DEFAULT '[]',
			obsoletes       JSONB DEFAULT '[]',
			files           JSONB DEFAULT '[]',
			changelogs      JSONB DEFAULT '[]',
			created_at      TIMESTAMPTZ DEFAULT NOW(),
			UNIQUE(repo_name, file_path)
		);

		CREATE INDEX IF NOT EXISTS idx_rpm_metadata_repo ON rpm_metadata(repo_name);

		ALTER TABLE rpm_metadata ADD COLUMN IF NOT EXISTS conflicts JSONB DEFAULT '[]';
		ALTER TABLE rpm_metadata ADD COLUMN IF NOT EXISTS obsoletes JSONB DEFAULT '[]';

		CREATE TABLE IF NOT EXISTS signing_keys (
			purpose             TEXT PRIMARY KEY,
			private_key_armor   TEXT NOT NULL,
			key_id              TEXT NOT NULL,
			created_at          TIMESTAMPTZ DEFAULT NOW()
		);
	`)
	return err
}
