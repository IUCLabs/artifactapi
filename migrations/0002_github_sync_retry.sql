-- Failed GitHub release scans retry with backoff instead of waiting out a full
-- mutable_ttl: sync_failures counts consecutive failures, next_retry_at gates
-- the next claim while a retry is pending.
ALTER TABLE github_rpm_sync_state    ADD COLUMN IF NOT EXISTS sync_failures INT NOT NULL DEFAULT 0;
ALTER TABLE github_rpm_sync_state    ADD COLUMN IF NOT EXISTS next_retry_at TIMESTAMPTZ;
ALTER TABLE github_deb_sync_state    ADD COLUMN IF NOT EXISTS sync_failures INT NOT NULL DEFAULT 0;
ALTER TABLE github_deb_sync_state    ADD COLUMN IF NOT EXISTS next_retry_at TIMESTAMPTZ;
ALTER TABLE github_alpine_sync_state ADD COLUMN IF NOT EXISTS sync_failures INT NOT NULL DEFAULT 0;
ALTER TABLE github_alpine_sync_state ADD COLUMN IF NOT EXISTS next_retry_at TIMESTAMPTZ;
