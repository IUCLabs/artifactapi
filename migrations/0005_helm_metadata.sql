CREATE TABLE IF NOT EXISTS helm_metadata (
	id           BIGSERIAL PRIMARY KEY,
	repo_name    TEXT NOT NULL,
	file_path    TEXT NOT NULL,
	content_hash TEXT NOT NULL,
	name         TEXT NOT NULL,
	version      TEXT NOT NULL,
	chart_yaml   TEXT NOT NULL,
	created_at   TIMESTAMPTZ DEFAULT NOW(),
	UNIQUE(repo_name, file_path)
);

CREATE INDEX IF NOT EXISTS idx_helm_metadata_repo ON helm_metadata(repo_name);
