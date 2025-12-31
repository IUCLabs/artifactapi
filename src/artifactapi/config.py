import os
import json
import yaml
from typing import Optional


class ConfigManager:
    def __init__(self, config_file: str = "remotes.yaml"):
        self.config_file = config_file
        self._last_modified = 0
        self.config = self._load_config()

    def _load_config(self) -> dict:
        try:
            with open(self.config_file, "r") as f:
                if self.config_file.endswith(".yaml") or self.config_file.endswith(
                    ".yml"
                ):
                    return yaml.safe_load(f)
                else:
                    return json.load(f)
        except FileNotFoundError:
            return {"remotes": {}}

    def _check_reload(self) -> None:
        """Check if config file has been modified and reload if needed"""
        try:
            import os

            current_modified = os.path.getmtime(self.config_file)
            if current_modified > self._last_modified:
                self._last_modified = current_modified
                self.config = self._load_config()
                print(f"Config reloaded from {self.config_file}")
        except OSError:
            pass

    def get_remote_config(self, remote_name: str) -> Optional[dict]:
        self._check_reload()
        return self.config.get("remotes", {}).get(remote_name)

    def get_repository_patterns(self, remote_name: str, repo_path: str) -> list:
        remote_config = self.get_remote_config(remote_name)
        if not remote_config:
            return []

        repositories = remote_config.get("repositories", {})

        # Handle both dict (GitHub style) and list (Alpine style) repositories
        if isinstance(repositories, dict):
            repo_config = repositories.get(repo_path)
            if repo_config:
                patterns = repo_config.get("include_patterns", [])
            else:
                patterns = remote_config.get("include_patterns", [])
        elif isinstance(repositories, list):
            # For Alpine, repositories is just a list of allowed repo names
            # Pattern matching is handled by the main include_patterns
            patterns = remote_config.get("include_patterns", [])
        else:
            patterns = remote_config.get("include_patterns", [])

        return patterns

    def get_s3_config(self) -> dict:
        """Get S3 configuration from environment variables"""
        endpoint = os.getenv("MINIO_ENDPOINT")
        access_key = os.getenv("MINIO_ACCESS_KEY")
        secret_key = os.getenv("MINIO_SECRET_KEY")
        bucket = os.getenv("MINIO_BUCKET")

        if not endpoint:
            raise ValueError("MINIO_ENDPOINT environment variable is required")
        if not access_key:
            raise ValueError("MINIO_ACCESS_KEY environment variable is required")
        if not secret_key:
            raise ValueError("MINIO_SECRET_KEY environment variable is required")
        if not bucket:
            raise ValueError("MINIO_BUCKET environment variable is required")

        return {
            "endpoint": endpoint,
            "access_key": access_key,
            "secret_key": secret_key,
            "bucket": bucket,
            "secure": os.getenv("MINIO_SECURE", "false").lower() == "true",
        }

    def get_redis_config(self) -> dict:
        """Get Redis configuration from environment variables"""
        redis_url = os.getenv("REDIS_URL")
        if not redis_url:
            raise ValueError("REDIS_URL environment variable is required")

        return {
            "url": redis_url
        }

    def get_database_config(self) -> dict:
        """Get database configuration from environment variables"""
        db_host = os.getenv("DBHOST")
        db_port = os.getenv("DBPORT")
        db_user = os.getenv("DBUSER")
        db_pass = os.getenv("DBPASS")
        db_name = os.getenv("DBNAME")

        if not all([db_host, db_port, db_user, db_pass, db_name]):
            missing = [var for var, val in [("DBHOST", db_host), ("DBPORT", db_port), ("DBUSER", db_user), ("DBPASS", db_pass), ("DBNAME", db_name)] if not val]
            raise ValueError(f"All database environment variables are required: {', '.join(missing)}")

        db_url = f"postgresql://{db_user}:{db_pass}@{db_host}:{db_port}/{db_name}"
        return {"url": db_url}

    def get_cache_config(self, remote_name: str) -> dict:
        """Get cache configuration for a specific remote"""
        remote_config = self.get_remote_config(remote_name)
        if not remote_config:
            return {}

        return remote_config.get("cache", {})
