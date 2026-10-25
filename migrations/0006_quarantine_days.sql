UPDATE remotes SET quarantine_days = 3 WHERE quarantine_days IS NULL OR quarantine_days <= 0;
