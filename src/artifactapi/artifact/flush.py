import logging

from fastapi import HTTPException

logger = logging.getLogger(__name__)


def handle(remote: str | None, cache_type: str, cache, storage) -> dict:
    try:
        result = {"remote": remote, "cache_type": cache_type, "flushed": {"redis_keys": 0, "s3_objects": 0, "operations": []}}

        if cache_type in ["all", "index", "metrics"] and cache.available and cache.client:
            patterns = []

            if cache_type in ["all", "index"]:
                if remote:
                    patterns += [f"index:{remote}:*", f"mutable:meta:{remote}:*"]
                else:
                    patterns += ["index:*", "mutable:meta:*"]

            if cache_type in ["all", "metrics"]:
                patterns.append(f"metrics:*:{remote}" if remote else "metrics:*")

            for pattern in patterns:
                keys = cache.client.keys(pattern)
                if keys:
                    cache.client.delete(*keys)
                    result["flushed"]["redis_keys"] += len(keys)
                    logger.info(f"Cache flush: deleted {len(keys)} Redis keys matching '{pattern}'")

            if result["flushed"]["redis_keys"] > 0:
                result["flushed"]["operations"].append(f"Deleted {result['flushed']['redis_keys']} Redis keys")

        if cache_type in ["all", "files"]:
            try:
                list_params = {"Bucket": storage.bucket}
                if remote:
                    list_params["Prefix"] = f"{remote}/"

                response = storage.client.list_objects_v2(**list_params)
                if "Contents" in response:
                    objects_to_delete = [obj["Key"] for obj in response["Contents"]]
                    for key in objects_to_delete:
                        try:
                            storage.client.delete_object(Bucket=storage.bucket, Key=key)
                            result["flushed"]["s3_objects"] += 1
                        except Exception as e:
                            logger.warning(f"Failed to delete S3 object {key}: {e}")

                    if objects_to_delete:
                        scope = f" for remote '{remote}'" if remote else ""
                        result["flushed"]["operations"].append(f"Deleted {len(objects_to_delete)} S3 objects{scope}")
                        logger.info(f"Cache flush: deleted {len(objects_to_delete)} S3 objects{scope}")

            except Exception as e:
                result["flushed"]["operations"].append(f"S3 flush failed: {str(e)}")
                logger.error(f"Cache flush S3 error: {e}")

        if not result["flushed"]["operations"]:
            result["flushed"]["operations"].append("No cache entries found to flush")

        return result

    except Exception as e:
        logger.error(f"Cache flush error: {e}")
        raise HTTPException(status_code=500, detail=f"Cache flush failed: {str(e)}")
