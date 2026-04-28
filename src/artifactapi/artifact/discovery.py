import logging
import re
from typing import Any
from urllib.parse import urlparse

import httpx
from fastapi import HTTPException

from .proxy import cache_single_artifact

logger = logging.getLogger(__name__)


async def _discover_github_releases(remote: str, include_pattern: str) -> list[str]:
    match = re.match(r"github\.com/([^/]+)/([^/]+)", remote)
    if not match:
        raise HTTPException(status_code=400, detail="Invalid GitHub remote format")

    owner, repo = match.groups()

    async with httpx.AsyncClient(follow_redirects=True) as client:
        response = await client.get(f"https://api.github.com/repos/{owner}/{repo}/releases")
        if response.status_code != 200:
            raise HTTPException(status_code=response.status_code, detail=f"Failed to fetch releases: {response.text}")

        releases = response.json()
        regex = re.compile(include_pattern.replace("*", ".*"))
        return [
            asset["browser_download_url"]
            for release in releases
            for asset in release.get("assets", [])
            if regex.search(asset["browser_download_url"])
        ]


async def _discover(remote: str, include_pattern: str) -> list[str]:
    if "github.com" in remote:
        return await _discover_github_releases(remote, include_pattern)
    raise HTTPException(status_code=400, detail=f"Unsupported remote: {remote}")


async def cache_artifacts(remote: str, include_pattern: str, storage) -> dict[str, Any]:
    try:
        matching_urls = await _discover(remote, include_pattern)

        if not matching_urls:
            return {"message": "No matching artifacts found", "cached_count": 0, "artifacts": []}

        cached_artifacts = []
        for url in matching_urls:
            result = await cache_single_artifact(url, "", "", storage, {})
            cached_artifacts.append(result)

        cached_count = sum(1 for a in cached_artifacts if a["status"] in ["cached", "already_cached"])
        return {
            "message": f"Processed {len(matching_urls)} artifacts, {cached_count} successfully cached",
            "cached_count": cached_count,
            "artifacts": cached_artifacts,
        }
    except Exception as e:
        raise HTTPException(status_code=500, detail=str(e))


async def list_artifacts(remote: str, include_pattern: str, storage) -> dict[str, Any]:
    try:
        matching_urls = await _discover(remote, include_pattern)
        cached_artifacts = []
        for url in matching_urls:
            parsed = urlparse(url)
            key = storage.get_object_key(remote, parsed.path)
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
