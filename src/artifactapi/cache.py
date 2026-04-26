import hashlib
import re
import time

import redis


class RedisCache:
    def __init__(self, redis_url: str):
        self.redis_url = redis_url

        try:
            self.client = redis.from_url(self.redis_url, decode_responses=True)
            # Test connection
            self.client.ping()
            self.available = True
        except Exception as e:
            print(f"Redis not available: {e}")
            self.client = None
            self.available = False

    def is_mutable_file(self, file_path: str, patterns: list[str] | None = None) -> bool:
        """Return True if file_path matches any of the mutable patterns."""
        if patterns is None:
            patterns = []
        return any(re.search(p, file_path) for p in patterns)

    def get_index_cache_key(self, remote_name: str, path: str) -> str:
        return f"index:{remote_name}:{hashlib.sha256(path.encode()).hexdigest()[:16]}"

    def get_mutable_meta_key(self, remote_name: str, path: str) -> str:
        return f"mutable:meta:{remote_name}:{hashlib.sha256(path.encode()).hexdigest()[:16]}"

    def is_index_valid(self, remote_name: str, path: str) -> bool:
        """Check if mutable file is still within its TTL window."""
        if not self.available:
            return False

        try:
            key = self.get_index_cache_key(remote_name, path)
            return self.client.exists(key) > 0
        except Exception:
            return False

    def mark_index_cached(self, remote_name: str, path: str, ttl: int = 300) -> None:
        """Set or refresh the TTL key for a mutable file."""
        if not self.available:
            return

        try:
            key = self.get_index_cache_key(remote_name, path)
            self.client.setex(key, ttl, str(int(time.time())))
        except Exception:
            pass

    def store_mutable_meta(self, remote_name: str, path: str, etag: str | None, last_modified: str | None) -> None:
        """Persist ETag and Last-Modified for future conditional requests."""
        if not self.available:
            return
        data = {}
        if etag:
            data["etag"] = etag
        if last_modified:
            data["last_modified"] = last_modified
        if not data:
            return
        try:
            self.client.hset(self.get_mutable_meta_key(remote_name, path), mapping=data)
        except Exception:
            pass

    def get_mutable_meta(self, remote_name: str, path: str) -> dict:
        """Return stored ETag/Last-Modified for a mutable file, or {}."""
        if not self.available:
            return {}
        try:
            return self.client.hgetall(self.get_mutable_meta_key(remote_name, path)) or {}
        except Exception:
            return {}

    def delete_mutable_meta(self, remote_name: str, path: str) -> None:
        if not self.available:
            return
        try:
            self.client.delete(self.get_mutable_meta_key(remote_name, path))
        except Exception:
            pass

    def cleanup_expired_index(self, storage, remote_name: str, path: str) -> None:
        """Remove an expired mutable file from S3 and clear its Redis meta."""
        if not self.available:
            return

        try:
            import os

            from .config import ConfigManager

            config_path = os.environ.get("CONFIG_PATH")
            if config_path:
                config = ConfigManager(config_path)
                remote_config = config.get_remote_config(remote_name)
                if remote_config:
                    base_url = remote_config.get("base_url")
                    if base_url:
                        s3_key = storage.get_object_key(remote_name, path)
                        if storage.exists(s3_key):
                            storage.client.delete_object(Bucket=storage.bucket, Key=s3_key)
        except Exception:
            pass

        self.delete_mutable_meta(remote_name, path)
