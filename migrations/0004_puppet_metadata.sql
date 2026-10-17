CREATE TABLE IF NOT EXISTS puppet_metadata (
	id            BIGSERIAL PRIMARY KEY,
	repo_name     TEXT NOT NULL,
	file_path     TEXT NOT NULL,
	content_hash  TEXT NOT NULL,
	slug          TEXT NOT NULL,
	version       TEXT NOT NULL,
	metadata_json TEXT NOT NULL,
	file_size     BIGINT NOT NULL DEFAULT 0,
	file_md5      TEXT NOT NULL,
	created_at    TIMESTAMPTZ DEFAULT NOW(),
	UNIQUE(repo_name, file_path)
);

CREATE INDEX IF NOT EXISTS idx_puppet_metadata_repo_slug ON puppet_metadata(repo_name, slug);
