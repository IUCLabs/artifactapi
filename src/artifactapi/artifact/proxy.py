import base64
import logging
import os
import re
from datetime import UTC, datetime, timedelta
from email.utils import parsedate_to_datetime

import httpx
from fastapi import HTTPException, Request, Response

from ..auth import get_docker_token_for_response
from ..remote import helm as _helm
from ..remote import npm as _npm
from ..remote import python as _pypi
from ..remote.base import get_content_type

logger = logging.getLogger(__name__)


class UpstreamUnreachable(Exception):
    """Raised when the upstream backend cannot be contacted (network or timeout error)."""


def _check_quarantine(remote_name: str, last_modified_str: str | None, config) -> None:
    """Raise HTTP 404 if the artifact is within the per-remote quarantine window.

    Fails open (allows the request) when the publish date cannot be determined.
    """
    enabled, days = config.get_quarantine_config(remote_name)
    if not enabled or not days:
        return
    if not last_modified_str:
        return  # cannot determine age → allow
    try:
        publish_date = parsedate_to_datetime(last_modified_str)
    except Exception:
        return  # unparseable → allow
    cutoff = datetime.now(UTC) - timedelta(days=days)
    if publish_date > cutoff:
        available_on = (publish_date + timedelta(days=days)).date()
        raise HTTPException(
            status_code=404,
            detail=(
                f"Package quarantined: published {publish_date.date()}, available after {available_on} ({days}-day new-release quarantine)"
            ),
        )


async def _fetch_last_modified(remote_url: str, remote_cfg: dict) -> str | None:
    """HEAD the upstream URL and return the Last-Modified header, or None on any failure."""
    auth = _basic_auth_header(remote_cfg)
    try:
        async with httpx.AsyncClient(follow_redirects=True) as client:
            response = await client.head(remote_url, headers=auth, timeout=10.0)
        return response.headers.get("Last-Modified")
    except Exception:
        return None


def _basic_auth_header(remote_cfg: dict) -> dict[str, str]:
    username = remote_cfg.get("username")
    password = remote_cfg.get("password")
    if username and password:
        token = base64.b64encode(f"{username}:{password}".encode()).decode()
        return {"Authorization": f"Basic {token}"}
    return {}


def _resolve_content(
    data: bytes,
    path: str,
    filename: str,
    remote_config: dict,
    request: Request,
    remote_name: str = "",
) -> tuple[bytes, str]:
    package = remote_config.get("package")
    proxy_base = str(request.base_url).rstrip("/")
    base_url = remote_config.get("base_url", "").rstrip("/")

    if package == "pypi":
        return _pypi.resolve_content(data, path, filename, remote_config.get("immutable_patterns", []), base_url, proxy_base, remote_name)
    if package == "npm":
        return _npm.resolve_content(data, path, filename, remote_config.get("immutable_patterns", []), base_url, proxy_base, remote_name)
    if package == "helm":
        return _helm.resolve_content(data, path, filename, base_url, proxy_base, remote_name)
    return data, get_content_type(filename)


def construct_url(remote_config: dict, path: str) -> str:
    base_url = remote_config.get("base_url", "").rstrip("/")
    if remote_config.get("package") == "docker":
        return f"{base_url}/v2/{path}"
    if remote_config.get("package") == "pypi":
        return _pypi.construct_url(base_url, path)
    return f"{base_url}/{path}"


async def cache_single_artifact(url: str, remote_name: str, path: str, storage, remote_config: dict) -> dict:
    key = storage.get_object_key(remote_name, path)

    if storage.exists(key):
        logger.info(f"Cache ALREADY EXISTS: {url} (key: {key})")
        return {"url": url, "cached_url": storage.get_url(key), "status": "already_cached"}

    try:
        is_docker = remote_config.get("package") == "docker" or "/v2/" in url
        headers = {}
        username = remote_config.get("username")
        password = remote_config.get("password")

        if is_docker:
            if "/manifests/" in url:
                headers["Accept"] = (
                    "application/vnd.docker.distribution.manifest.v2+json,"
                    "application/vnd.oci.image.manifest.v1+json,"
                    "application/vnd.oci.image.index.v1+json,"
                    "application/vnd.docker.distribution.manifest.list.v2+json"
                )
            elif "/blobs/" in url:
                headers["Accept"] = "application/octet-stream"
        elif username and password:
            headers["Authorization"] = "Basic " + base64.b64encode(f"{username}:{password}".encode()).decode()

        async with httpx.AsyncClient(follow_redirects=True) as client:
            response = await client.get(url, headers=headers)

            if response.status_code == 401 and is_docker:
                www_auth = response.headers.get("WWW-Authenticate", "")
                token = await get_docker_token_for_response(www_auth, username, password)
                if token:
                    headers["Authorization"] = f"Bearer {token}"
                    response = await client.get(url, headers=headers)

            response.raise_for_status()
            storage.upload(key, response.content)
            logger.info(f"Cache ADD SUCCESS: {url} (size: {len(response.content)} bytes, key: {key})")

            return {
                "url": url,
                "cached_url": storage.get_url(key),
                "storage_path": f"s3://{storage.bucket}/{key}",
                "size": len(response.content),
                "status": "cached",
                "etag": response.headers.get("ETag"),
                "last_modified": response.headers.get("Last-Modified"),
            }

    except Exception as e:
        return {"url": url, "status": "error", "error": str(e)}


async def _upstream_reachable(url: str, auth_headers: dict | None = None) -> bool:
    try:
        async with httpx.AsyncClient(follow_redirects=True) as client:
            await client.head(url, headers=auth_headers or {}, timeout=10.0)
        return True
    except (httpx.NetworkError, httpx.TimeoutException):
        return False
    except Exception:
        return True


async def check_upstream_changed(remote_url: str, remote_name: str, path: str, cache, auth_headers: dict | None = None) -> bool:
    meta = cache.get_mutable_meta(remote_name, path)
    if not meta:
        return True

    headers = dict(auth_headers or {})
    if meta.get("etag"):
        headers["If-None-Match"] = meta["etag"]
    if meta.get("last_modified"):
        headers["If-Modified-Since"] = meta["last_modified"]
    if not (meta.get("etag") or meta.get("last_modified")):
        return True

    try:
        async with httpx.AsyncClient(follow_redirects=True) as client:
            response = await client.head(remote_url, headers=headers)
        return response.status_code != 304
    except (httpx.NetworkError, httpx.TimeoutException) as exc:
        raise UpstreamUnreachable(str(exc)) from exc


async def handle_expired_mutable(remote_name: str, path: str, remote_url: str, config, cache, storage) -> bool:
    """Handle an expired mutable file. Returns True if the cached copy is still valid."""
    mutable_ttl = config.get_cache_config(remote_name).get("mutable_ttl", 3600)
    remote_cfg = config.get_remote_config(remote_name) or {}
    auth = _basic_auth_header(remote_cfg)
    check_updates = remote_cfg.get("check_mutable_updates", False)
    user_mutable = check_updates and cache.is_mutable_file(path, config.get_user_mutable_patterns(remote_name))

    if user_mutable:
        try:
            changed = await check_upstream_changed(remote_url, remote_name, path, cache, auth)
        except UpstreamUnreachable:
            cache.mark_index_cached(remote_name, path, mutable_ttl)
            logger.warning(f"Mutable STALE (backend unreachable): {remote_name}/{path} - TTL extended ({mutable_ttl}s)")
            return True
        if not changed:
            cache.mark_index_cached(remote_name, path, mutable_ttl)
            logger.info(f"Mutable file UNCHANGED: {remote_name}/{path} - TTL refreshed ({mutable_ttl}s)")
            return True
        logger.info(f"Mutable file CHANGED: {remote_name}/{path} - re-downloading")
    else:
        if not await _upstream_reachable(remote_url, auth):
            cache.mark_index_cached(remote_name, path, mutable_ttl)
            logger.warning(f"Mutable STALE (backend unreachable): {remote_name}/{path} - TTL extended ({mutable_ttl}s)")
            return True
        logger.info(f"Mutable file EXPIRED: {remote_name}/{path} - removing from cache")

    cache.cleanup_expired_index(storage, remote_name, path)
    return False


async def handle(request: Request, remote_name: str, path: str, storage, cache, config, database, metrics) -> Response:
    remote_config = config.get_remote_config(remote_name)
    if not remote_config:
        raise HTTPException(status_code=404, detail=f"Remote '{remote_name}' not configured")

    path_parts = path.split("/")
    if len(path_parts) >= 2:
        repo_path = f"{path_parts[0]}/{path_parts[1]}"
        file_path = "/".join(path_parts[2:])
    else:
        repo_path = path
        file_path = path

    mutable_patterns = config.get_mutable_patterns(remote_name)
    if not cache.is_mutable_file(file_path, mutable_patterns) and not cache.is_mutable_file(path, mutable_patterns):
        patterns = config.get_immutable_patterns(remote_name, repo_path)
        if patterns and not any(re.search(p, file_path) or re.search(p, path) for p in patterns):
            logger.info(f"PATTERN BLOCKED: {remote_name}/{path} - not matching include patterns")
            raise HTTPException(status_code=403, detail="Artifact not allowed by configuration patterns")

    remote_url = construct_url(remote_config, path)
    if not remote_config.get("base_url"):
        raise HTTPException(status_code=500, detail=f"No base_url configured for remote '{remote_name}'")

    cached_key = storage.get_object_key(remote_name, path)
    if not storage.exists(cached_key):
        cached_key = None

    filename = os.path.basename(path)
    is_mutable = cache.is_mutable_file(path, mutable_patterns)

    if cached_key and is_mutable:
        if not cache.is_index_valid(remote_name, path):
            if not await handle_expired_mutable(remote_name, path, remote_url, config, cache, storage):
                cached_key = None

    if cached_key:
        if not is_mutable:
            published = cache.get_artifact_published(remote_name, path)
            if not published:
                published = await _fetch_last_modified(remote_url, remote_config)
                if published:
                    cache.store_artifact_published(remote_name, path, published)
            _check_quarantine(remote_name, published, config)

        try:
            artifact_data = storage.download_object(cached_key)
            artifact_data, content_type = _resolve_content(artifact_data, path, filename, remote_config, request, remote_name)
            logger.info(f"Cache HIT: {remote_name}/{path} (size: {len(artifact_data)} bytes, key: {cached_key})")
            metrics.record_cache_hit(remote_name, len(artifact_data))
            database.record_artifact_mapping(cached_key, remote_name, path, len(artifact_data))
            return Response(
                content=artifact_data,
                media_type=content_type,
                headers={
                    "Content-Disposition": f"attachment; filename={filename}",
                    "X-Artifact-Source": "cache",
                    "X-Artifact-Size": str(len(artifact_data)),
                },
            )
        except HTTPException:
            raise
        except Exception as e:
            raise HTTPException(status_code=500, detail=f"Error retrieving cached artifact: {str(e)}")

    logger.info(f"Cache MISS: {remote_name}/{path} - fetching from remote: {remote_url}")
    result = await cache_single_artifact(remote_url, remote_name, path, storage, remote_config)

    if result["status"] == "error":
        logger.error(f"Cache ADD FAILED: {remote_name}/{path} - {result['error']}")
        raise HTTPException(status_code=502, detail=f"Failed to fetch artifact: {result['error']}")

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
        _check_quarantine(remote_name, published, config)

    try:
        cache_key = storage.get_object_key(remote_name, path)
        artifact_data = storage.download_object(cache_key)
        artifact_data, content_type = _resolve_content(artifact_data, path, filename, remote_config, request, remote_name)
        metrics.record_cache_miss(remote_name, len(artifact_data))
        database.record_artifact_mapping(cache_key, remote_name, path, len(artifact_data))
        return Response(
            content=artifact_data,
            media_type=content_type,
            headers={
                "Content-Disposition": f"attachment; filename={filename}",
                "X-Artifact-Source": "remote",
                "X-Artifact-Size": str(len(artifact_data)),
            },
        )
    except Exception as e:
        raise HTTPException(status_code=500, detail=f"Error serving artifact: {str(e)}")
