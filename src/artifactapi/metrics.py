from datetime import datetime
from typing import Dict, Any
from prometheus_client import Counter, Gauge


# Prometheus metrics
request_counter = Counter(
    "artifact_requests_total", "Total artifact requests", ["remote", "status"]
)
cache_hit_counter = Counter("artifact_cache_hits_total", "Total cache hits", ["remote"])
cache_miss_counter = Counter(
    "artifact_cache_misses_total", "Total cache misses", ["remote"]
)
bandwidth_saved_counter = Counter(
    "artifact_bandwidth_saved_bytes_total", "Total bandwidth saved", ["remote"]
)
storage_size_gauge = Gauge(
    "artifact_storage_size_bytes", "Storage size by remote", ["remote"]
)
redis_keys_gauge = Gauge("artifact_redis_keys_total", "Total Redis keys")


class MetricsManager:
    def __init__(self, redis_client=None, database_manager=None):
        self.redis_client = redis_client
        self.database_manager = database_manager
        self.start_time = datetime.now()

    def record_cache_hit(self, remote_name: str, size_bytes: int):
        """Record a cache hit with size for bandwidth calculation"""
        # Update Prometheus metrics
        request_counter.labels(remote=remote_name, status="cache_hit").inc()
        cache_hit_counter.labels(remote=remote_name).inc()
        bandwidth_saved_counter.labels(remote=remote_name).inc(size_bytes)

        # Update Redis for persistence across instances
        if self.redis_client and self.redis_client.available:
            try:
                # Increment global counters
                self.redis_client.client.incr("metrics:cache_hits")
                self.redis_client.client.incr("metrics:total_requests")
                self.redis_client.client.incrby("metrics:bandwidth_saved", size_bytes)

                # Increment per-remote counters
                self.redis_client.client.incr(f"metrics:cache_hits:{remote_name}")
                self.redis_client.client.incr(f"metrics:total_requests:{remote_name}")
                self.redis_client.client.incrby(
                    f"metrics:bandwidth_saved:{remote_name}", size_bytes
                )
            except Exception:
                pass

    def record_cache_miss(self, remote_name: str, size_bytes: int):
        """Record a cache miss (new download)"""
        # Update Prometheus metrics
        request_counter.labels(remote=remote_name, status="cache_miss").inc()
        cache_miss_counter.labels(remote=remote_name).inc()

        # Update Redis for persistence across instances
        if self.redis_client and self.redis_client.available:
            try:
                # Increment global counters
                self.redis_client.client.incr("metrics:cache_misses")
                self.redis_client.client.incr("metrics:total_requests")

                # Increment per-remote counters
                self.redis_client.client.incr(f"metrics:cache_misses:{remote_name}")
                self.redis_client.client.incr(f"metrics:total_requests:{remote_name}")
            except Exception:
                pass

    def get_redis_key_count(self) -> int:
        """Get total number of keys in Redis"""
        if self.redis_client and self.redis_client.available:
            try:
                return self.redis_client.client.dbsize()
            except Exception:
                return 0
        return 0

    def get_s3_total_size(self, storage) -> int:
        """Get total size of all objects in S3 bucket"""
        try:
            total_size = 0
            paginator = storage.client.get_paginator("list_objects_v2")
            for page in paginator.paginate(Bucket=storage.bucket):
                if "Contents" in page:
                    for obj in page["Contents"]:
                        total_size += obj["Size"]
            return total_size
        except Exception:
            return 0

    def get_s3_size_by_remote(self, storage, config_manager) -> Dict[str, int]:
        """Get size of stored data per remote using database mappings"""
        if self.database_manager and self.database_manager.available:
            # Get from database if available
            db_sizes = self.database_manager.get_storage_by_remote()
            if db_sizes:
                # Initialize all configured remotes to 0
                remote_sizes = {}
                for remote in config_manager.config.get("remotes", {}).keys():
                    remote_sizes[remote] = db_sizes.get(remote, 0)

                # Update Prometheus gauges
                for remote, size in remote_sizes.items():
                    storage_size_gauge.labels(remote=remote).set(size)

                return remote_sizes

        # Fallback to S3 scanning if database not available
        try:
            remote_sizes = {}
            remotes = config_manager.config.get("remotes", {}).keys()

            # Initialize all remotes to 0
            for remote in remotes:
                remote_sizes[remote] = 0

            paginator = storage.client.get_paginator("list_objects_v2")
            for page in paginator.paginate(Bucket=storage.bucket):
                if "Contents" in page:
                    for obj in page["Contents"]:
                        key = obj["Key"]
                        # Try to map from database first
                        remote = None
                        if self.database_manager:
                            remote = self.database_manager.get_remote_for_s3_key(key)

                        # Fallback to key parsing
                        if not remote:
                            remote = key.split("/")[0] if "/" in key else "unknown"

                        if remote in remote_sizes:
                            remote_sizes[remote] += obj["Size"]
                        else:
                            remote_sizes.setdefault("unknown", 0)
                            remote_sizes["unknown"] += obj["Size"]

            # Update Prometheus gauges
            for remote, size in remote_sizes.items():
                if remote != "unknown":  # Don't set gauge for unknown
                    storage_size_gauge.labels(remote=remote).set(size)

            return remote_sizes
        except Exception:
            return {}

    def get_metrics(self, storage, config_manager) -> Dict[str, Any]:
        """Get comprehensive metrics"""
        # Update Redis keys gauge
        redis_key_count = self.get_redis_key_count()
        redis_keys_gauge.set(redis_key_count)

        metrics = {
            "timestamp": datetime.now().isoformat(),
            "uptime_seconds": int((datetime.now() - self.start_time).total_seconds()),
            "redis": {"total_keys": redis_key_count},
            "storage": {
                "total_size_bytes": self.get_s3_total_size(storage),
                "size_by_remote": self.get_s3_size_by_remote(storage, config_manager),
            },
            "requests": {
                "cache_hits": 0,
                "cache_misses": 0,
                "total_requests": 0,
                "cache_hit_ratio": 0.0,
            },
            "bandwidth": {"saved_bytes": 0},
            "per_remote": {},
        }

        if self.redis_client and self.redis_client.available:
            try:
                # Get global metrics
                cache_hits = int(
                    self.redis_client.client.get("metrics:cache_hits") or 0
                )
                cache_misses = int(
                    self.redis_client.client.get("metrics:cache_misses") or 0
                )
                total_requests = cache_hits + cache_misses
                bandwidth_saved = int(
                    self.redis_client.client.get("metrics:bandwidth_saved") or 0
                )

                metrics["requests"]["cache_hits"] = cache_hits
                metrics["requests"]["cache_misses"] = cache_misses
                metrics["requests"]["total_requests"] = total_requests
                metrics["requests"]["cache_hit_ratio"] = (
                    cache_hits / total_requests if total_requests > 0 else 0.0
                )
                metrics["bandwidth"]["saved_bytes"] = bandwidth_saved

                # Get per-remote metrics
                for remote in config_manager.config.get("remotes", {}).keys():
                    remote_cache_hits = int(
                        self.redis_client.client.get(f"metrics:cache_hits:{remote}")
                        or 0
                    )
                    remote_cache_misses = int(
                        self.redis_client.client.get(f"metrics:cache_misses:{remote}")
                        or 0
                    )
                    remote_total = remote_cache_hits + remote_cache_misses
                    remote_bandwidth_saved = int(
                        self.redis_client.client.get(
                            f"metrics:bandwidth_saved:{remote}"
                        )
                        or 0
                    )

                    metrics["per_remote"][remote] = {
                        "cache_hits": remote_cache_hits,
                        "cache_misses": remote_cache_misses,
                        "total_requests": remote_total,
                        "cache_hit_ratio": remote_cache_hits / remote_total
                        if remote_total > 0
                        else 0.0,
                        "bandwidth_saved_bytes": remote_bandwidth_saved,
                        "storage_size_bytes": metrics["storage"]["size_by_remote"].get(
                            remote, 0
                        ),
                    }

            except Exception:
                pass

        return metrics
