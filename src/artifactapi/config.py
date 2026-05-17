import glob
import json
import os

import yaml

_PACKAGE_MUTABLE_PATTERNS: dict[str, list[str]] = {
    "alpine": [
        r"APKINDEX\.tar\.gz$",
    ],
    "rpm": [
        r"repomd\.xml$",
        r"repodata/.*\.(xml|xml\.gz|xml\.bz2|xml\.xz|xml\.zck|xml\.zst"
        r"|sqlite|sqlite\.gz|sqlite\.bz2|sqlite\.xz|sqlite\.zck|sqlite\.zst"
        r"|yaml\.xz|yaml\.gz|yaml\.bz2|yaml\.zst|asc|txt)$",
        r"Packages\.gz$",
    ],
    "docker": [
        r"/manifests/(?!sha256:)[^/]+$",
        r"/tags/list$",
    ],
    "pypi": [
        r"simple/",  # Per-package and top-level simple index pages
    ],
    "npm": [],
    "helm": [
        r"index\.yaml$",
    ],
    "puppet": [
        r"^v3/modules/",
        r"^v3/releases",
    ],
    "generic": [],
}


class ConfigManager:
    def __init__(self, config_path: str = "remotes.yaml"):
        self.config_path = config_path
        self._config_dir: str | None = None
        self._last_modified: float = 0.0
        self.config = self._load_config()

    def _load_single_file(self, path: str) -> dict:
        try:
            with open(path) as f:
                if path.endswith((".yaml", ".yml")):
                    return yaml.safe_load(f) or {}
                return json.load(f)
        except FileNotFoundError:
            return {}

    @staticmethod
    def _merge(base: dict, overlay: dict) -> dict:
        result = {**base}
        for key, value in overlay.items():
            if key in ("remotes", "virtuals", "locals") and isinstance(base.get(key), dict) and isinstance(value, dict):
                result[key] = {**base.get(key, {}), **value}
            else:
                result[key] = value
        return result

    def _load_from_dir(self, dir_path: str) -> dict:
        merged: dict = {}
        files = sorted(glob.glob(os.path.join(dir_path, "*.yaml")) + glob.glob(os.path.join(dir_path, "*.yml")))
        for path in files:
            merged = self._merge(merged, self._load_single_file(path))
        return merged

    def _load_config(self) -> dict:
        self._config_dir = None

        if os.path.isdir(self.config_path):
            return self._load_from_dir(self.config_path) or {"remotes": {}, "virtuals": {}, "locals": {}}

        config = self._load_single_file(self.config_path)
        if not config:
            return {"remotes": {}, "virtuals": {}, "locals": {}}

        config_dir = config.pop("config_dir", None)
        if config_dir:
            if not os.path.isabs(config_dir):
                config_dir = os.path.join(os.path.dirname(os.path.abspath(self.config_path)), config_dir)
            self._config_dir = config_dir
            config = self._merge(config, self._load_from_dir(config_dir))

        return config

    def _file_mtimes(self) -> list[float]:
        mtimes: list[float] = []
        if os.path.isdir(self.config_path):
            for f in glob.glob(os.path.join(self.config_path, "*.yaml")) + glob.glob(os.path.join(self.config_path, "*.yml")):
                try:
                    mtimes.append(os.path.getmtime(f))
                except OSError:
                    pass
        else:
            try:
                mtimes.append(os.path.getmtime(self.config_path))
            except OSError:
                pass

        if self._config_dir and os.path.isdir(self._config_dir):
            for f in glob.glob(os.path.join(self._config_dir, "*.yaml")) + glob.glob(os.path.join(self._config_dir, "*.yml")):
                try:
                    mtimes.append(os.path.getmtime(f))
                except OSError:
                    pass

        return mtimes

    def _check_reload(self) -> None:
        try:
            current_modified = max(self._file_mtimes(), default=0.0)
            if current_modified > self._last_modified:
                self._last_modified = current_modified
                self.config = self._load_config()
                print(f"Config reloaded from {self.config_path}")
        except OSError:
            pass

    def get_remote_config(self, remote_name: str) -> dict | None:
        self._check_reload()
        return self.config.get("remotes", {}).get(remote_name)

    def get_virtual_config(self, virtual_name: str) -> dict | None:
        self._check_reload()
        return self.config.get("virtuals", {}).get(virtual_name)

    def get_local_config(self, local_name: str) -> dict | None:
        self._check_reload()
        return self.config.get("locals", {}).get(local_name)

    def get_immutable_patterns(self, remote_name: str, repo_path: str = "") -> list[str]:
        remote_config = self.get_remote_config(remote_name)
        if not remote_config:
            return []

        repositories = remote_config.get("repositories", {})

        if isinstance(repositories, dict):
            repo_config = repositories.get(repo_path)
            if repo_config:
                patterns = repo_config.get("immutable_patterns", [])
            else:
                patterns = remote_config.get("immutable_patterns", [])
        else:
            patterns = remote_config.get("immutable_patterns", [])

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

        return {"url": redis_url}

    def get_database_config(self) -> dict:
        """Get database configuration from environment variables"""
        db_host = os.getenv("DBHOST")
        db_port = os.getenv("DBPORT")
        db_user = os.getenv("DBUSER")
        db_pass = os.getenv("DBPASS")
        db_name = os.getenv("DBNAME")

        if not all([db_host, db_port, db_user, db_pass, db_name]):
            missing = [
                var
                for var, val in [("DBHOST", db_host), ("DBPORT", db_port), ("DBUSER", db_user), ("DBPASS", db_pass), ("DBNAME", db_name)]
                if not val
            ]
            raise ValueError(f"All database environment variables are required: {', '.join(missing)}")

        db_url = f"postgresql://{db_user}:{db_pass}@{db_host}:{db_port}/{db_name}"
        return {"url": db_url}

    def get_user_mutable_patterns(self, remote_name: str) -> list[str]:
        """Return only user-configured mutable_patterns, excluding package-type defaults."""
        remote_config = self.get_remote_config(remote_name)
        if not remote_config:
            return []
        return remote_config.get("mutable_patterns", [])

    def get_mutable_patterns(self, remote_name: str) -> list[str]:
        """Return mutable-file patterns for a remote (TTL is configured per-remote in cache.index_ttl).

        Merges the package-level defaults with any extra patterns listed under
        ``mutable_patterns`` in the remote's config.
        """
        remote_config = self.get_remote_config(remote_name)
        if not remote_config:
            return []
        package = remote_config.get("package", "generic")
        defaults = _PACKAGE_MUTABLE_PATTERNS.get(package, [])
        extra = remote_config.get("mutable_patterns", [])
        return defaults + [p for p in extra if p not in defaults]

    def get_cache_config(self, remote_name: str) -> dict:
        """Get cache configuration for a specific remote"""
        remote_config = self.get_remote_config(remote_name)
        if not remote_config:
            return {}

        return remote_config.get("cache", {})

    def get_quarantine_config(self, remote_name: str) -> tuple[bool, int]:
        """Return (enabled, quarantine_days) for a remote.

        When enabled=True and quarantine_days>0, immutable artifacts published
        within the last quarantine_days days are blocked with a 404.
        """
        remote_config = self.get_remote_config(remote_name)
        if not remote_config:
            return False, 0
        enabled = bool(remote_config.get("quarantine_new", False))
        days = int(remote_config.get("quarantine_days", 0))
        return enabled, days
