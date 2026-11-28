CREATE TABLE IF NOT EXISTS github_apps (
    name TEXT PRIMARY KEY,
    app_id BIGINT NOT NULL CHECK (app_id > 0),
    installation_id BIGINT NOT NULL CHECK (installation_id > 0),
    private_key TEXT NOT NULL,
    private_key_fingerprint TEXT NOT NULL,
    api_base_url TEXT NOT NULL DEFAULT 'https://api.github.com',
    description TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

ALTER TABLE remotes ADD COLUMN IF NOT EXISTS github_app TEXT
    REFERENCES github_apps(name) ON DELETE RESTRICT;

CREATE INDEX IF NOT EXISTS remotes_github_app_idx ON remotes (github_app) WHERE github_app IS NOT NULL;
