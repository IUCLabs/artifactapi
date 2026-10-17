-- Sync state is dropped with its remote, so a recreated remote of the same name
-- starts with no inherited ETag or timing. NOT VALID skips checking rows left by
-- remotes deleted before this migration, which would otherwise fail it.
ALTER TABLE github_rpm_sync_state    DROP CONSTRAINT IF EXISTS github_rpm_sync_state_remote_fk;
ALTER TABLE github_rpm_sync_state    ADD CONSTRAINT github_rpm_sync_state_remote_fk
	FOREIGN KEY (remote_name) REFERENCES remotes(name) ON DELETE CASCADE NOT VALID;
ALTER TABLE github_deb_sync_state    DROP CONSTRAINT IF EXISTS github_deb_sync_state_remote_fk;
ALTER TABLE github_deb_sync_state    ADD CONSTRAINT github_deb_sync_state_remote_fk
	FOREIGN KEY (remote_name) REFERENCES remotes(name) ON DELETE CASCADE NOT VALID;
ALTER TABLE github_alpine_sync_state DROP CONSTRAINT IF EXISTS github_alpine_sync_state_remote_fk;
ALTER TABLE github_alpine_sync_state ADD CONSTRAINT github_alpine_sync_state_remote_fk
	FOREIGN KEY (remote_name) REFERENCES remotes(name) ON DELETE CASCADE NOT VALID;
