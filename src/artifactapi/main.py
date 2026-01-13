import os
import re
import hashlib
import logging
from typing import Dict, Any, Optional
import httpx
from fastapi import FastAPI, HTTPException, Response, Query, File, UploadFile
from fastapi.responses import PlainTextResponse, JSONResponse
from pydantic import BaseModel
from prometheus_client import generate_latest, CONTENT_TYPE_LATEST

from .config import ConfigManager
from .database import DatabaseManager
from .storage import S3Storage
from .cache import RedisCache
from .metrics import MetricsManager


class ArtifactRequest(BaseModel):
    remote: str
    include_pattern: str


# Configure logging
logging.basicConfig(
    level=logging.INFO,
    format='%(asctime)s - %(name)s - %(levelname)s - %(message)s'
)
logger = logging.getLogger(__name__)

app = FastAPI(title="Artifact Storage API", version="2.0.1")

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


async def construct_remote_url(remote_name: str, path: str) -> str:
    remote_config = config.get_remote_config(remote_name)
    if not remote_config:
        raise HTTPException(
            status_code=404, detail=f"Remote '{remote_name}' not configured"
        )

    base_url = remote_config.get("base_url")
    if not base_url:
        raise HTTPException(
            status_code=500, detail=f"No base_url configured for remote '{remote_name}'"
        )

    return f"{base_url}/{path}"


async def check_artifact_patterns(
    remote_name: str, repo_path: str, file_path: str, full_path: str
) -> bool:
    # First check if this is an index file - always allow index files
    if cache.is_index_file(file_path) or cache.is_index_file(full_path):
        return True

    # Then check basic include patterns
    patterns = config.get_repository_patterns(remote_name, repo_path)
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

    # All remotes now use pattern-based filtering only - no additional checks needed
    return True


async def cache_single_artifact(url: str, remote_name: str, path: str) -> dict:
    # Check if using URL-based key or path-based key
    if url.startswith("http"):
        key = storage.get_object_key(url)
    else:
        key = storage.get_object_key_from_path(remote_name, path)

    if storage.exists(key):
        logger.info(f"Cache ALREADY EXISTS: {url} (key: {key})")
        return {
            "url": url,
            "cached_url": storage.get_url(key),
            "status": "already_cached",
        }

    try:
        async with httpx.AsyncClient(follow_redirects=True) as client:
            response = await client.get(url)
            response.raise_for_status()

            storage_path = storage.upload(key, response.content)

            logger.info(f"Cache ADD SUCCESS: {url} (size: {len(response.content)} bytes, key: {key})")

            return {
                "url": url,
                "cached_url": storage.get_url(key),
                "storage_path": storage_path,
                "size": len(response.content),
                "status": "cached",
            }

    except Exception as e:
        return {"url": url, "status": "error", "error": str(e)}


@app.get("/api/v1/remote/{remote_name}/{path:path}")
async def get_artifact(remote_name: str, path: str):
    # Check if remote is configured
    remote_config = config.get_remote_config(remote_name)
    if not remote_config:
        raise HTTPException(
            status_code=404, detail=f"Remote '{remote_name}' not configured"
        )

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
            headers={
                "Content-Disposition": f"attachment; filename={os.path.basename(path)}"
            },
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
        raise HTTPException(
            status_code=403, detail="Artifact not allowed by configuration patterns"
        )

    # Construct the remote URL
    remote_url = await construct_remote_url(remote_name, path)

    # Check if artifact is already cached (try both URL and path-based keys)
    url_key = storage.get_object_key(remote_url)
    path_key = storage.get_object_key_from_path(remote_name, path)

    cached_key = None
    if storage.exists(url_key):
        cached_key = url_key
    elif storage.exists(path_key):
        cached_key = path_key

    # For index files, check Redis TTL validity
    filename = os.path.basename(path)
    is_index = cache.is_index_file(filename)

    if cached_key and is_index:
        # Index file exists, but check if it's still valid
        if not cache.is_index_valid(remote_name, path):
            # Index has expired, remove it from S3
            logger.info(f"Index EXPIRED: {remote_name}/{path} - removing from cache")
            cache.cleanup_expired_index(storage, remote_name, path)
            cached_key = None  # Force re-download

    if cached_key:
        # Return cached artifact
        try:
            artifact_data = storage.download_object(cached_key)
            filename = os.path.basename(path)

            # Log cache hit
            logger.info(f"Cache HIT: {remote_name}/{path} (size: {len(artifact_data)} bytes, key: {cached_key})")

            # Determine content type based on file extension
            content_type = "application/octet-stream"
            if filename.endswith(".tar.gz"):
                content_type = "application/gzip"
            elif filename.endswith(".zip"):
                content_type = "application/zip"
            elif filename.endswith(".exe"):
                content_type = "application/x-msdownload"
            elif filename.endswith(".rpm"):
                content_type = "application/x-rpm"
            elif filename.endswith(".xml"):
                content_type = "application/xml"
            elif filename.endswith((".xml.gz", ".xml.bz2", ".xml.xz")):
                content_type = "application/gzip"

            # Record cache hit metrics
            metrics.record_cache_hit(remote_name, len(artifact_data))

            # Record artifact mapping in database if not already recorded
            database.record_artifact_mapping(
                cached_key, remote_name, path, len(artifact_data)
            )

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
            raise HTTPException(
                status_code=500, detail=f"Error retrieving cached artifact: {str(e)}"
            )

    # Artifact not cached, cache it first
    logger.info(f"Cache MISS: {remote_name}/{path} - fetching from remote: {remote_url}")
    result = await cache_single_artifact(remote_url, remote_name, path)

    if result["status"] == "error":
        logger.error(f"Cache ADD FAILED: {remote_name}/{path} - {result['error']}")
        raise HTTPException(
            status_code=502, detail=f"Failed to fetch artifact: {result['error']}"
        )

    # Mark index files as cached in Redis if this was a new download
    if result["status"] == "cached" and is_index:
        # Get TTL from remote config
        cache_config = config.get_cache_config(remote_name)
        index_ttl = cache_config.get("index_ttl", 300)  # Default 5 minutes
        cache.mark_index_cached(remote_name, path, index_ttl)
        logger.info(f"Index file cached with TTL: {remote_name}/{path} (ttl: {index_ttl}s)")

    # Now return the cached artifact
    try:
        cache_key = storage.get_object_key(remote_url)
        artifact_data = storage.download_object(cache_key)
        filename = os.path.basename(path)

        content_type = "application/octet-stream"
        if filename.endswith(".tar.gz"):
            content_type = "application/gzip"
        elif filename.endswith(".zip"):
            content_type = "application/zip"
        elif filename.endswith(".exe"):
            content_type = "application/x-msdownload"
        elif filename.endswith(".rpm"):
            content_type = "application/x-rpm"
        elif filename.endswith(".xml"):
            content_type = "application/xml"
        elif filename.endswith((".xml.gz", ".xml.bz2", ".xml.xz")):
            content_type = "application/gzip"

        # Record cache miss metrics
        metrics.record_cache_miss(remote_name, len(artifact_data))

        # Record artifact mapping in database
        cache_key = storage.get_object_key(remote_url)
        database.record_artifact_mapping(
            cache_key, remote_name, path, len(artifact_data)
        )

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
        response = await client.get(
            f"https://api.github.com/repos/{owner}/{repo}/releases"
        )

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
        raise HTTPException(
            status_code=404, detail=f"Remote '{remote_name}' not configured"
        )

    if remote_config.get("type") != "local":
        raise HTTPException(
            status_code=400, detail="Upload only supported for local repositories"
        )

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
        raise HTTPException(
            status_code=404, detail=f"Remote '{remote_name}' not configured"
        )

    # Handle local repository
    if remote_config.get("type") == "local":
        try:
            metadata = database.get_local_file_metadata(remote_name, path)
            if not metadata:
                raise HTTPException(status_code=404, detail="File not found")

            return Response(
                headers={
                    "Content-Length": str(metadata["size_bytes"]),
                    "Content-Type": metadata.get(
                        "content_type", "application/octet-stream"
                    ),
                    "X-SHA256": metadata["sha256_sum"],
                    "X-Created-At": metadata["created_at"].isoformat()
                    if metadata["created_at"]
                    else "",
                    "X-Uploaded-At": metadata["uploaded_at"].isoformat()
                    if metadata["uploaded_at"]
                    else "",
                }
            )
        except HTTPException:
            raise
        except Exception as e:
            raise HTTPException(status_code=500, detail=f"Check failed: {str(e)}")
    else:
        # For remote repositories, just return 405 Method Not Allowed
        raise HTTPException(
            status_code=405, detail="HEAD method only supported for local repositories"
        )


@app.delete("/api/v1/remote/{remote_name}/{path:path}")
def delete_file(remote_name: str, path: str):
    """Delete a file from local repository"""
    # Check if remote is configured and is local
    remote_config = config.get_remote_config(remote_name)
    if not remote_config:
        raise HTTPException(
            status_code=404, detail=f"Remote '{remote_name}' not configured"
        )

    if remote_config.get("type") != "local":
        raise HTTPException(
            status_code=400, detail="Delete only supported for local repositories"
        )

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
async def cache_artifact(request: ArtifactRequest) -> Dict[str, Any]:
    try:
        matching_urls = await discover_artifacts(
            request.remote, request.include_pattern
        )

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

        cached_count = sum(
            1
            for artifact in cached_artifacts
            if artifact["status"] in ["cached", "already_cached"]
        )

        return {
            "message": f"Processed {len(matching_urls)} artifacts, {cached_count} successfully cached",
            "cached_count": cached_count,
            "artifacts": cached_artifacts,
        }

    except Exception as e:
        raise HTTPException(status_code=500, detail=str(e))


@app.get("/api/v1/artifacts/{remote:path}")
async def list_cached_artifacts(
    remote: str, include_pattern: str = ".*"
) -> Dict[str, Any]:
    try:
        matching_urls = await discover_artifacts(remote, include_pattern)

        cached_artifacts = []
        for url in matching_urls:
            key = storage.get_object_key(url)
            if storage.exists(key):
                cached_artifacts.append(
                    {"url": url, "cached_url": storage.get_url(key), "key": key}
                )

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
    json: Optional[bool] = Query(
        False, description="Return JSON format instead of Prometheus"
    ),
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
