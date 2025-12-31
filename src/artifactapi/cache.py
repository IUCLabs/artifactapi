import time
import hashlib
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

    def is_index_file(self, file_path: str) -> bool:
        """Check if the file is an index file that should have TTL"""
        return (
            file_path.endswith("APKINDEX.tar.gz")
            or file_path.endswith("Packages.gz")
            or file_path.endswith("repomd.xml")
            or "repodata/" in file_path
            and file_path.endswith((".xml", ".xml.gz", ".xml.bz2", ".xml.xz"))
        )

    def get_index_cache_key(self, remote_name: str, path: str) -> str:
        """Generate cache key for index files"""
        return f"index:{remote_name}:{hashlib.sha256(path.encode()).hexdigest()[:16]}"

    def is_index_valid(
        self, remote_name: str, path: str, ttl_override: int = None
    ) -> bool:
        """Check if index file is still valid (not expired)"""
        if not self.available:
            return False

        try:
            key = self.get_index_cache_key(remote_name, path)
            return self.client.exists(key) > 0
        except Exception:
            return False

    def mark_index_cached(self, remote_name: str, path: str, ttl: int = 300) -> None:
        """Mark index file as cached with TTL"""
        if not self.available:
            return

        try:
            key = self.get_index_cache_key(remote_name, path)
            self.client.setex(key, ttl, str(int(time.time())))
        except Exception:
            pass

    def cleanup_expired_index(self, storage, remote_name: str, path: str) -> None:
        """Remove expired index from S3 storage"""
        if not self.available:
            return

        try:
            # Get the S3 key and remove it
            s3_key = storage.get_object_key_from_path(remote_name, path)
            if storage.exists(s3_key):
                storage.client.delete_object(Bucket=storage.bucket, Key=s3_key)
        except Exception:
            pass
