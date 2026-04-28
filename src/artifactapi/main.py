import base64
import hashlib
import json
import logging
import os
import re
from typing import Any

import httpx
from fastapi import FastAPI, File, HTTPException, Query, Request, Response, UploadFile
from fastapi.responses import JSONResponse, PlainTextResponse
from prometheus_client import CONTENT_TYPE_LATEST, generate_latest
from pydantic import BaseModel

try:
    from importlib.metadata import version

    __version__ = version("artifactapi")
except ImportError:
    # Fallback for development when package isn't installed
    __version__ = "dev"

from .auth import get_docker_token_for_response
from .cache import RedisCache
from .config import ConfigManager
from .database import DatabaseManager
from .metrics import MetricsManager
from .remote import helm as _helm
from .remote import npm as _npm
from .remote import python as _pypi
from .remote.base import get_content_type as _get_content_type
from .storage import S3Storage


class ArtifactRequest(BaseModel):
    remote: str
    include_pattern: str


class UpstreamUnreachable(Exception):
    """Raised when the upstream backend cannot be contacted (network or timeout error)."""


# Configure logging
logging.basicConfig(level=logging.INFO, format="%(asctime)s - %(name)s - %(levelname)s - %(message)s")
logger = logging.getLogger(__name__)

app = FastAPI(title="Artifact Storage API", version=__version__)

# Initialize components using config
config_path = os.environ.get("CONFIG_PATH")
if not config_path:
    raise ValueError("CONFIG_PATH environment variable is required")
config = ConfigManager(config_path)

# Get configurations
s3_config = config.get_s3_config()
redis_config = config.get_redis_config()
db_config = config.get_database_config()

# Initialize services
storage = S3Storage(**s3_config)
cache = RedisCache(redis_config["url"])
database = DatabaseManager(db_config["url"])
metrics = MetricsManager(cache, database)


@app.get("/")
def read_root():
    config._check_reload()
    return {
        "message": "Artifact Storage API",
        "version": app.version,
        "remotes": list(config.config.get("remotes", {}).keys()),
    }


@app.get("/health")
def health_check():
    return {"status": "healthy"}


@app.put("/cache/flush")
def flush_cache(
    remote: str = Query(default=None, description="Specific remote to flush (optional)"),
    cache_type: str = Query(default="all", description="Type to flush: 'all', 'index', 'files', 'metrics'"),
):
    """Flush cache entries for specified remote or all remotes"""
    try:
        result = {"remote": remote, "cache_type": cache_type, "flushed": {"redis_keys": 0, "s3_objects": 0, "operations": []}}

        # Flush Redis entries based on cache_type
        if cache_type in ["all", "index", "metrics"] and cache.available and cache.client:
            patterns = []

            if cache_type in ["all", "index"]:
                if remote:
                    patterns.append(f"index:{remote}:*")
                    patterns.append(f"mutable:meta:{remote}:*")
                else:
                    patterns.append("index:*")
                    patterns.append("mutable:meta:*")

            if cache_type in ["all", "metrics"]:
                if remote:
                    patterns.append(f"metrics:*:{remote}")
                else:
                    patterns.append("metrics:*")

            for pattern in patterns:
                keys = cache.client.keys(pattern)
                if keys:
                    cache.client.delete(*keys)
                    result["flushed"]["redis_keys"] += len(keys)
                    logger.info(f"Cache flush: Deleted {len(keys)} Redis keys matching '{pattern}'")

            if result["flushed"]["redis_keys"] > 0:
                result["flushed"]["operations"].append(f"Deleted {result['flushed']['redis_keys']} Redis keys")

        # Flush S3 objects if requested
        if cache_type in ["all", "files"]:
            try:
                # Use prefix filtering for remote-specific deletion
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
                        logger.info(f"Cache flush: Deleted {len(objects_to_delete)} S3 objects{scope}")

            except Exception as e:
                result["flushed"]["operations"].append(f"S3 flush failed: {str(e)}")
                logger.error(f"Cache flush S3 error: {e}")

        if not result["flushed"]["operations"]:
            result["flushed"]["operations"].append("No cache entries found to flush")

        return result

    except Exception as e:
        logger.error(f"Cache flush error: {e}")
        raise HTTPException(status_code=500, detail=f"Cache flush failed: {str(e)}")


async def construct_remote_url(remote_name: str, path: str) -> str:
    remote_config = config.get_remote_config(remote_name)
    if not remote_config:
        raise HTTPException(status_code=404, detail=f"Remote '{remote_name}' not configured")

    base_url = remote_config.get("base_url")
    if not base_url:
        raise HTTPException(status_code=500, detail=f"No base_url configured for remote '{remote_name}'")

    if remote_config.get("package") == "docker":
        return f"{base_url}/v2/{path}"

    if remote_config.get("package") == "pypi":
        return _pypi.construct_url(base_url, path)

    return f"{base_url}/{path}"


async def check_artifact_patterns(remote_name: str, repo_path: str, file_path: str, full_path: str) -> bool:
    # Mutable files (index files) are always allowed through
    mutable_patterns = config.get_mutable_patterns(remote_name)
    if cache.is_mutable_file(file_path, mutable_patterns) or cache.is_mutable_file(full_path, mutable_patterns):
        return True

    # Check immutable include patterns
    patterns = config.get_immutable_patterns(remote_name, repo_path)
    if not patterns:
        return True  # Allow all if no patterns configured

    pattern_matched = False
    for pattern in patterns:
        # Check both file_path and full_path to handle different pattern types
        if re.search(pattern, file_path) or re.search(pattern, full_path):
            pattern_matched = True
            break

    if not pattern_matched:
        return False

    return True


async def cache_single_artifact(url: str, remote_name: str, path: str) -> dict:
    # Use hierarchical path-based key
    key = storage.get_object_key(remote_name, path)

    if storage.exists(key):
        logger.info(f"Cache ALREADY EXISTS: {url} (key: {key})")
        return {
            "url": url,
            "cached_url": storage.get_url(key),
            "status": "already_cached",
        }

    try:
        remote_config = config.get_remote_config(remote_name) or {}
        is_docker = remote_config.get("package") == "docker" or "/v2/" in url

        # Prepare headers
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

            # Handle Docker Bearer token challenge
            if response.status_code == 401 and is_docker:
                www_auth = response.headers.get("WWW-Authenticate", "")
                username = remote_config.get("username")
                password = remote_config.get("password")
                token = await get_docker_token_for_response(www_auth, username, password)
                if token:
                    headers["Authorization"] = f"Bearer {token}"
                    response = await client.get(url, headers=headers)

            response.raise_for_status()

            storage_path = storage.upload(key, response.content)

            logger.info(f"Cache ADD SUCCESS: {url} (size: {len(response.content)} bytes, key: {key})")

            return {
                "url": url,
                "cached_url": storage.get_url(key),
                "storage_path": storage_path,
                "size": len(response.content),
                "status": "cached",
                "etag": response.headers.get("ETag"),
                "last_modified": response.headers.get("Last-Modified"),
            }

    except Exception as e:
        return {"url": url, "status": "error", "error": str(e)}


def _basic_auth_header(remote_cfg: dict) -> dict[str, str]:
    username = remote_cfg.get("username")
    password = remote_cfg.get("password")
    if username and password:
        token = base64.b64encode(f"{username}:{password}".encode()).decode()
        return {"Authorization": f"Basic {token}"}
    return {}


async def _upstream_reachable(url: str, auth_headers: dict | None = None) -> bool:
    """HEAD with a short timeout. Returns False only on network/timeout errors."""
    try:
        async with httpx.AsyncClient(follow_redirects=True) as client:
            await client.head(url, headers=auth_headers or {}, timeout=10.0)
        return True
    except (httpx.NetworkError, httpx.TimeoutException):
        return False
    except Exception:
        return True  # 4xx/5xx means backend is up


async def check_upstream_changed(remote_url: str, remote_name: str, path: str, auth_headers: dict | None = None) -> bool:
    """Conditional HEAD against upstream. Returns False only on a definitive 304.
    Raises UpstreamUnreachable if the backend cannot be contacted."""
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


async def handle_expired_mutable(remote_name: str, path: str, remote_url: str) -> bool:
    """Handle an expired mutable file. Returns True if the cached copy is still valid."""
    mutable_ttl = config.get_cache_config(remote_name).get("mutable_ttl", 3600)

    remote_cfg = config.get_remote_config(remote_name) or {}
    auth = _basic_auth_header(remote_cfg)
    check_updates = remote_cfg.get("check_mutable_updates", False)
    user_mutable = check_updates and cache.is_mutable_file(path, config.get_user_mutable_patterns(remote_name))

    if user_mutable:
        try:
            changed = await check_upstream_changed(remote_url, remote_name, path, auth)
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


def _resolve_content(
    data: bytes,
    path: str,
    filename: str,
    remote_config: dict,
    request: Request,
    remote_name: str = "",
) -> tuple[bytes, str]:
    """Return (possibly-rewritten data, content_type) for a cached artifact."""
    package = remote_config.get("package")
    proxy_base = str(request.base_url).rstrip("/")
    base_url = remote_config.get("base_url", "").rstrip("/")

    if package == "pypi":
        return _pypi.resolve_content(data, path, filename, remote_config.get("immutable_patterns", []), base_url, proxy_base, remote_name)
    if package == "npm":
        return _npm.resolve_content(data, path, filename, remote_config.get("immutable_patterns", []), base_url, proxy_base, remote_name)
    if package == "helm":
        return _helm.resolve_content(data, path, filename, base_url, proxy_base, remote_name)
    return data, _get_content_type(filename)


@app.get("/api/v1/remote/{remote_name}/{path:path}")
async def get_artifact(request: Request, remote_name: str, path: str):
    # Check if remote is configured
    remote_config = config.get_remote_config(remote_name)
    if not remote_config:
        raise HTTPException(status_code=404, detail=f"Remote '{remote_name}' not configured")

    # Check if this is a local repository
    if remote_config.get("type") == "local":
        # Handle local repository download
        metadata = database.get_local_file_metadata(remote_name, path)
        if not metadata:
            raise HTTPException(status_code=404, detail="File not found")

        # Get file from S3
        content = storage.download_object(metadata["s3_key"])
        if content is None:
            raise HTTPException(status_code=500, detail="File not accessible")

        # Determine content type
        content_type = metadata.get("content_type", "application/octet-stream")

        return Response(
            content=content,
            media_type=content_type,
            headers={"Content-Disposition": f"attachment; filename={os.path.basename(path)}"},
        )

    # Extract repository path for pattern checking
    path_parts = path.split("/")
    if len(path_parts) >= 2:
        repo_path = f"{path_parts[0]}/{path_parts[1]}"
        file_path = "/".join(path_parts[2:])
    else:
        repo_path = path
        file_path = path

    # Check if artifact matches configured patterns
    if not await check_artifact_patterns(remote_name, repo_path, file_path, path):
        logger.info(f"PATTERN BLOCKED: {remote_name}/{path} - not matching include patterns")
        raise HTTPException(status_code=403, detail="Artifact not allowed by configuration patterns")

    # Construct the remote URL
    remote_url = await construct_remote_url(remote_name, path)

    # Check if artifact is already cached
    cached_key = storage.get_object_key(remote_name, path)
    if not storage.exists(cached_key):
        cached_key = None

    # For mutable files, check Redis TTL validity
    filename = os.path.basename(path)
    is_mutable = cache.is_mutable_file(path, config.get_mutable_patterns(remote_name))

    if cached_key and is_mutable:
        if not cache.is_index_valid(remote_name, path):
            if not await handle_expired_mutable(remote_name, path, remote_url):
                cached_key = None

    if cached_key:
        # Return cached artifact
        try:
            artifact_data = storage.download_object(cached_key)
            filename = os.path.basename(path)
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
        except Exception as e:
            raise HTTPException(status_code=500, detail=f"Error retrieving cached artifact: {str(e)}")

    # Artifact not cached, cache it first
    logger.info(f"Cache MISS: {remote_name}/{path} - fetching from remote: {remote_url}")
    result = await cache_single_artifact(remote_url, remote_name, path)

    if result["status"] == "error":
        logger.error(f"Cache ADD FAILED: {remote_name}/{path} - {result['error']}")
        raise HTTPException(status_code=502, detail=f"Failed to fetch artifact: {result['error']}")

    # Mark mutable files as cached in Redis with TTL
    if result["status"] == "cached" and is_mutable:
        cache_config = config.get_cache_config(remote_name)
        mutable_ttl = cache_config.get("mutable_ttl", 3600)
        cache.mark_index_cached(remote_name, path, mutable_ttl)
        logger.info(f"Mutable file cached with TTL: {remote_name}/{path} (ttl: {mutable_ttl}s)")
        if result.get("etag") or result.get("last_modified"):
            cache.store_mutable_meta(remote_name, path, result.get("etag"), result.get("last_modified"))

    # Now return the cached artifact
    try:
        cache_key = storage.get_object_key(remote_name, path)
        artifact_data = storage.download_object(cache_key)
        filename = os.path.basename(path)
        artifact_data, content_type = _resolve_content(artifact_data, path, filename, remote_config, request, remote_name)

        metrics.record_cache_miss(remote_name, len(artifact_data))
        cache_key = storage.get_object_key(remote_name, path)
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


@app.get("/v2/")
async def docker_v2_ping():
    return Response(
        content="{}",
        media_type="application/json",
        headers={"Docker-Distribution-Api-Version": "registry/2.0"},
    )


@app.api_route("/v2/{remote_name}/{path:path}", methods=["GET", "HEAD"])
async def docker_v2_proxy(request: Request, remote_name: str, path: str):
    remote_config = config.get_remote_config(remote_name)
    if not remote_config:
        raise HTTPException(status_code=404, detail=f"Remote '{remote_name}' not configured")
    if remote_config.get("package") != "docker":
        raise HTTPException(status_code=400, detail=f"Remote '{remote_name}' is not a docker remote")

    # Check immutable_patterns against the image name (e.g. "library/nginx")
    patterns = config.get_immutable_patterns(remote_name, "")
    if patterns:
        path_parts = path.split("/")
        image_name = "/".join(path_parts[:2]) if len(path_parts) >= 2 else path
        if not any(re.search(p, path) or re.search(p, image_name) for p in patterns):
            logger.info(f"PATTERN BLOCKED: {remote_name}/{path}")
            raise HTTPException(status_code=403, detail="Image not allowed by configuration patterns")

    remote_url = await construct_remote_url(remote_name, path)

    cached_key = storage.get_object_key(remote_name, path)
    if not storage.exists(cached_key):
        cached_key = None

    is_mutable = cache.is_mutable_file(path, config.get_mutable_patterns(remote_name))

    if cached_key and is_mutable:
        if not cache.is_index_valid(remote_name, path):
            if not await handle_expired_mutable(remote_name, path, remote_url):
                cached_key = None

    if not cached_key:
        logger.info(f"Cache MISS: {remote_name}/{path} - fetching from remote: {remote_url}")
        result = await cache_single_artifact(remote_url, remote_name, path)
        if result["status"] == "error":
            raise HTTPException(status_code=502, detail=f"Failed to fetch: {result['error']}")
        if result["status"] == "cached" and is_mutable:
            cache_config = config.get_cache_config(remote_name)
            mutable_ttl = cache_config.get("mutable_ttl", 3600)
            cache.mark_index_cached(remote_name, path, mutable_ttl)
            logger.info(f"Mutable file cached with TTL: {remote_name}/{path} (ttl: {mutable_ttl}s)")
            if result.get("etag") or result.get("last_modified"):
                cache.store_mutable_meta(remote_name, path, result.get("etag"), result.get("last_modified"))

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


async def discover_artifacts(remote: str, include_pattern: str) -> list[str]:
    if "github.com" in remote:
        return await discover_github_releases(remote, include_pattern)
    else:
        raise HTTPException(status_code=400, detail=f"Unsupported remote: {remote}")


async def discover_github_releases(remote: str, include_pattern: str) -> list[str]:
    match = re.match(r"github\.com/([^/]+)/([^/]+)", remote)
    if not match:
        raise HTTPException(status_code=400, detail="Invalid GitHub remote format")

    owner, repo = match.groups()

    async with httpx.AsyncClient(follow_redirects=True) as client:
        response = await client.get(f"https://api.github.com/repos/{owner}/{repo}/releases")

        if response.status_code != 200:
            raise HTTPException(
                status_code=response.status_code,
                detail=f"Failed to fetch releases: {response.text}",
            )

        releases = response.json()

        matching_urls = []
        pattern = include_pattern.replace("*", ".*")
        regex = re.compile(pattern)

        for release in releases:
            for asset in release.get("assets", []):
                download_url = asset["browser_download_url"]
                if regex.search(download_url):
                    matching_urls.append(download_url)

        return matching_urls


@app.put("/api/v1/remote/{remote_name}/{path:path}")
async def upload_file(remote_name: str, path: str, file: UploadFile = File(...)):
    """Upload a file to local repository"""
    # Check if remote is configured and is local
    remote_config = config.get_remote_config(remote_name)
    if not remote_config:
        raise HTTPException(status_code=404, detail=f"Remote '{remote_name}' not configured")

    if remote_config.get("type") != "local":
        raise HTTPException(status_code=400, detail="Upload only supported for local repositories")

    try:
        # Read file content
        content = await file.read()

        # Calculate SHA256
        sha256_sum = hashlib.sha256(content).hexdigest()

        # Check if file already exists (prevent overwrite)
        if database.file_exists(remote_name, path):
            raise HTTPException(status_code=409, detail="File already exists")

        # Generate S3 key
        s3_key = f"local/{remote_name}/{path}"

        # Determine content type
        content_type = file.content_type or "application/octet-stream"

        # Upload to S3
        try:
            storage.upload(s3_key, content)
        except Exception as e:
            raise HTTPException(status_code=500, detail=f"Upload failed: {e}")

        # Add to database
        success = database.add_local_file(
            repository_name=remote_name,
            file_path=path,
            s3_key=s3_key,
            size_bytes=len(content),
            sha256_sum=sha256_sum,
            content_type=content_type,
        )

        if not success:
            # Clean up S3 if database insert failed
            storage.delete_object(s3_key)
            raise HTTPException(status_code=500, detail="Failed to save file metadata")

        return JSONResponse(
            {
                "message": "File uploaded successfully",
                "file_path": path,
                "size_bytes": len(content),
                "sha256_sum": sha256_sum,
                "content_type": content_type,
            }
        )

    except HTTPException:
        raise
    except Exception as e:
        raise HTTPException(status_code=500, detail=f"Upload failed: {str(e)}")


@app.head("/api/v1/remote/{remote_name}/{path:path}")
def check_file_exists(remote_name: str, path: str):
    """Check if file exists (for CI jobs) - supports local repositories only"""
    # Check if remote is configured
    remote_config = config.get_remote_config(remote_name)
    if not remote_config:
        raise HTTPException(status_code=404, detail=f"Remote '{remote_name}' not configured")

    # Handle local repository
    if remote_config.get("type") == "local":
        try:
            metadata = database.get_local_file_metadata(remote_name, path)
            if not metadata:
                raise HTTPException(status_code=404, detail="File not found")

            return Response(
                headers={
                    "Content-Length": str(metadata["size_bytes"]),
                    "Content-Type": metadata.get("content_type", "application/octet-stream"),
                    "X-SHA256": metadata["sha256_sum"],
                    "X-Created-At": metadata["created_at"].isoformat() if metadata["created_at"] else "",
                    "X-Uploaded-At": metadata["uploaded_at"].isoformat() if metadata["uploaded_at"] else "",
                }
            )
        except HTTPException:
            raise
        except Exception as e:
            raise HTTPException(status_code=500, detail=f"Check failed: {str(e)}")
    else:
        # For remote repositories, just return 405 Method Not Allowed
        raise HTTPException(status_code=405, detail="HEAD method only supported for local repositories")


@app.delete("/api/v1/remote/{remote_name}/{path:path}")
def delete_file(remote_name: str, path: str):
    """Delete a file from local repository"""
    # Check if remote is configured and is local
    remote_config = config.get_remote_config(remote_name)
    if not remote_config:
        raise HTTPException(status_code=404, detail=f"Remote '{remote_name}' not configured")

    if remote_config.get("type") != "local":
        raise HTTPException(status_code=400, detail="Delete only supported for local repositories")

    try:
        # Get S3 key before deleting from database
        s3_key = database.delete_local_file(remote_name, path)
        if not s3_key:
            raise HTTPException(status_code=404, detail="File not found")

        # Delete from S3
        if not storage.delete_object(s3_key):
            # File was deleted from database but not from S3 - log warning but continue
            print(f"Warning: Failed to delete S3 object {s3_key}")

        return JSONResponse({"message": "File deleted successfully"})
    except HTTPException:
        raise
    except Exception as e:
        raise HTTPException(status_code=500, detail=f"Delete failed: {str(e)}")


@app.post("/api/v1/artifacts/cache")
async def cache_artifact(request: ArtifactRequest) -> dict[str, Any]:
    try:
        matching_urls = await discover_artifacts(request.remote, request.include_pattern)

        if not matching_urls:
            return {
                "message": "No matching artifacts found",
                "cached_count": 0,
                "artifacts": [],
            }

        cached_artifacts = []

        for url in matching_urls:
            result = await cache_single_artifact(url, "", "")
            cached_artifacts.append(result)

        cached_count = sum(1 for artifact in cached_artifacts if artifact["status"] in ["cached", "already_cached"])

        return {
            "message": f"Processed {len(matching_urls)} artifacts, {cached_count} successfully cached",
            "cached_count": cached_count,
            "artifacts": cached_artifacts,
        }

    except Exception as e:
        raise HTTPException(status_code=500, detail=str(e))


@app.get("/api/v1/artifacts/{remote:path}")
async def list_cached_artifacts(remote: str, include_pattern: str = ".*") -> dict[str, Any]:
    try:
        matching_urls = await discover_artifacts(remote, include_pattern)

        cached_artifacts = []
        for url in matching_urls:
            # Extract path from URL for hierarchical key generation
            from urllib.parse import urlparse

            parsed = urlparse(url)
            path = parsed.path
            key = storage.get_object_key(remote, path)
            if storage.exists(key):
                cached_artifacts.append({"url": url, "cached_url": storage.get_url(key), "key": key})

        return {
            "remote": remote,
            "pattern": include_pattern,
            "total_found": len(matching_urls),
            "cached_count": len(cached_artifacts),
            "artifacts": cached_artifacts,
        }

    except Exception as e:
        raise HTTPException(status_code=500, detail=str(e))


@app.get("/metrics")
def get_metrics(
    json: bool | None = Query(False, description="Return JSON format instead of Prometheus"),
):
    """Get comprehensive metrics about the artifact storage system"""
    config._check_reload()

    if json:
        # Return JSON format
        return metrics.get_metrics(storage, config)
    else:
        # Return Prometheus format
        metrics.get_metrics(storage, config)  # Update gauges
        prometheus_data = generate_latest().decode("utf-8")
        return PlainTextResponse(prometheus_data, media_type=CONTENT_TYPE_LATEST)


@app.get("/config")
def get_config():
    return config.config


def main():
    import uvicorn

    uvicorn.run(app, host="0.0.0.0", port=8000)


if __name__ == "__main__":
    main()
