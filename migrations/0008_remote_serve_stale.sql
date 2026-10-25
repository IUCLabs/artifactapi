-- serve_stale lets a virtual merge a member's previous index while that member
-- refreshes in the background.
ALTER TABLE remotes ADD COLUMN IF NOT EXISTS serve_stale BOOLEAN NOT NULL DEFAULT FALSE;
