import hashlib
import json
import logging
import re

from fastapi import HTTPException, Request, Response

from . import proxy as _proxy

logger = logging.getLogger(__name__)


def ping() -> Response:
    return Response(
        content="{}",
        media_type="application/json",
        headers={"Docker-Distribution-Api-Version": "registry/2.0"},
    )


async def proxy(request: Request, remote_name: str, path: str, storage, cache, config, metrics) -> Response:
    remote_config = config.get_remote_config(remote_name)
    if not remote_config:
        raise HTTPException(status_code=404, detail=f"Remote '{remote_name}' not configured")
    if remote_config.get("package") != "docker":
        raise HTTPException(status_code=400, detail=f"Remote '{remote_name}' is not a docker remote")

    patterns = config.get_immutable_patterns(remote_name, "")
    if patterns:
        path_parts = path.split("/")
        image_name = "/".join(path_parts[:2]) if len(path_parts) >= 2 else path
        if not any(re.search(p, path) or re.search(p, image_name) for p in patterns):
            logger.info(f"PATTERN BLOCKED: {remote_name}/{path}")
            raise HTTPException(status_code=403, detail="Image not allowed by configuration patterns")

    base_url = remote_config.get("base_url", "").rstrip("/")
    remote_url = f"{base_url}/v2/{path}"

    cached_key = storage.get_object_key(remote_name, path)
    if not storage.exists(cached_key):
        cached_key = None

    is_mutable = cache.is_mutable_file(path, config.get_mutable_patterns(remote_name))

    if cached_key and is_mutable:
        if not cache.is_index_valid(remote_name, path):
            if not await _proxy.handle_expired_mutable(remote_name, path, remote_url, config, cache, storage):
                cached_key = None

    if not cached_key:
        logger.info(f"Cache MISS: {remote_name}/{path} - fetching from remote: {remote_url}")
        result = await _proxy.cache_single_artifact(remote_url, remote_name, path, storage, remote_config)
        if result["status"] == "error":
            raise HTTPException(status_code=502, detail=f"Failed to fetch: {result['error']}")
        if result["status"] == "cached" and is_mutable:
            cache_config = config.get_cache_config(remote_name)
            mutable_ttl = cache_config.get("mutable_ttl", 3600)
            cache.mark_index_cached(remote_name, path, mutable_ttl)
            logger.info(f"Mutable file cached with TTL: {remote_name}/{path} (ttl: {mutable_ttl}s)")
            if result.get("etag") or result.get("last_modified"):
                cache.store_mutable_meta(remote_name, path, result.get("etag"), result.get("last_modified"))
        if not is_mutable:
            published = result.get("last_modified")
            if published:
                cache.store_artifact_published(remote_name, path, published)
            _proxy._check_quarantine(remote_name, published, config)
    elif not is_mutable:
        published = cache.get_artifact_published(remote_name, path)
        if not published:
            published = await _proxy._fetch_last_modified(remote_url, remote_config)
            if published:
                cache.store_artifact_published(remote_name, path, published)
        _proxy._check_quarantine(remote_name, published, config)

    artifact_data = storage.download_object(storage.get_object_key(remote_name, path))

    is_blob = "/blobs/" in path
    if is_blob:
        content_type = "application/octet-stream"
    else:
        try:
            manifest_json = json.loads(artifact_data)
            content_type = manifest_json.get("mediaType")
            if not content_type:
                if "manifests" in manifest_json:
                    content_type = "application/vnd.oci.image.index.v1+json"
                else:
                    content_type = "application/vnd.oci.image.manifest.v1+json"
        except Exception:
            content_type = "application/vnd.oci.image.manifest.v1+json"

    digest = f"sha256:{hashlib.sha256(artifact_data).hexdigest()}"
    headers = {
        "Docker-Distribution-Api-Version": "registry/2.0",
        "Docker-Content-Digest": digest,
        "Content-Length": str(len(artifact_data)),
    }

    if request.method == "HEAD":
        return Response(status_code=200, headers=headers, media_type=content_type)

    metrics.record_cache_hit(remote_name, len(artifact_data))
    return Response(content=artifact_data, media_type=content_type, headers=headers)
