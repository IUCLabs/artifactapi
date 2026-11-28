ALTER TABLE github_apps ALTER COLUMN installation_id DROP NOT NULL;
ALTER TABLE github_apps DROP CONSTRAINT IF EXISTS github_apps_installation_id_check;
ALTER TABLE github_apps ADD CONSTRAINT github_apps_installation_id_check CHECK (installation_id IS NULL OR installation_id > 0);
