UPDATE remotes SET repo_type = CASE WHEN COALESCE(base_url, '') = '' THEN 'local' ELSE 'remote' END WHERE repo_type = '' OR repo_type IS NULL;
