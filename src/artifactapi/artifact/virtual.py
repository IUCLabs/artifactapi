import asyncio
import base64
import logging
import time
from datetime import UTC, date, datetime
from typing import Protocol, runtime_checkable

import httpx
import yaml
from fastapi import HTTPException, Request, Response

from ..remote import helm as _helm

logger = logging.getLogger(__name__)


class _HelmDumper(yaml.Dumper):
    """YAML dumper that serializes datetime/date objects back to ISO 8601 strings.

    yaml.safe_load converts timestamp-shaped YAML scalars (e.g. chart `created`
    fields) to Python datetime objects. Without a custom representer, yaml.dump
    would render them as "2022-12-16 11:08:49+00:00" (space, not T), which
    Go's YAML parser cannot unmarshal into time.Time.
    """


def _repr_datetime(dumper: yaml.Dumper, data: datetime) -> yaml.ScalarNode:
    s = data.strftime("%Y-%m-%dT%H:%M:%S.%f") + ("Z" if data.tzinfo else "")
    return dumper.represent_scalar("tag:yaml.org,2002:str", s)


def _repr_date(dumper: yaml.Dumper, data: date) -> yaml.ScalarNode:
    return dumper.represent_scalar("tag:yaml.org,2002:str", data.isoformat())


_HelmDumper.add_representer(datetime, _repr_datetime)
_HelmDumper.add_representer(date, _repr_date)


async def _get_member_index(
    member_name: str,
    member_cfg: dict,
    path: str,
    storage,
    cache,
) -> tuple[str, dict, int, bytes | None]:
    """Fetch or retrieve cached index.yaml for one member remote.

    Returns (member_name, member_cfg, ttl, raw_bytes).
    raw_bytes is None if the member is unreachable and not in S3.
    """
    member_ttl = member_cfg.get("cache", {}).get("mutable_ttl", 3600)
    s3_key = storage.get_object_key(member_name, path)
    raw_data: bytes | None = None

    if storage.exists(s3_key) and cache.is_index_valid(member_name, path):
        try:
            raw_data = storage.download_object(s3_key)
            logger.info(f"Virtual: cache hit for member '{member_name}'")
        except Exception:
            raw_data = None

    if raw_data is None:
        base_url = member_cfg.get("base_url", "").rstrip("/")
        upstream_url = f"{base_url}/index.yaml"
        headers = {}
        username = member_cfg.get("username")
        password = member_cfg.get("password")
        if username and password:
            token = base64.b64encode(f"{username}:{password}".encode()).decode()
            headers["Authorization"] = f"Basic {token}"
        try:
            async with httpx.AsyncClient(follow_redirects=True) as client:
                response = await client.get(upstream_url, headers=headers, timeout=30.0)
                response.raise_for_status()
                raw_data = response.content
        except Exception as e:
            logger.warning(f"Virtual: failed to fetch index.yaml from member '{member_name}': {e}")
            return member_name, member_cfg, member_ttl, None
        try:
            storage.upload(s3_key, raw_data)
            cache.mark_index_cached(member_name, path, member_ttl)
        except Exception as e:
            logger.warning(f"Virtual: failed to cache index.yaml for member '{member_name}': {e}")

    return member_name, member_cfg, member_ttl, raw_data


def _merge_helm_indexes(raw_indexes: list[bytes], member_names: list[str], member_configs: list[dict], proxy_base: str) -> bytes:
    """Merge helm index.yaml files with per-member URL rewriting.

    Priority is determined by position in member_names: earlier members win
    when the same chart name + version appears in multiple remotes.
    """
    merged_entries: dict[str, list] = {}

    for raw_data, member_name, member_cfg in zip(raw_indexes, member_names, member_configs):
        base_url = member_cfg.get("base_url", "").rstrip("/")
        rewritten, _ = _helm.resolve_content(raw_data, "index.yaml", "index.yaml", base_url, proxy_base, member_name)

        try:
            index = yaml.safe_load(rewritten)
        except Exception as e:
            logger.warning(f"Virtual: failed to parse index.yaml from member '{member_name}': {e}")
            continue

        for chart_name, versions in (index.get("entries") or {}).items():
            if chart_name not in merged_entries:
                merged_entries[chart_name] = list(versions)
            else:
                existing = {(v.get("name"), v.get("version")) for v in merged_entries[chart_name]}
                for version_entry in versions:
                    key = (version_entry.get("name"), version_entry.get("version"))
                    if key not in existing:
                        merged_entries[chart_name].append(version_entry)
                        existing.add(key)

    merged = {
        "apiVersion": "v1",
        "entries": merged_entries,
        "generated": datetime.now(UTC).strftime("%Y-%m-%dT%H:%M:%S.000Z"),
    }
    return yaml.dump(merged, Dumper=_HelmDumper, default_flow_style=False, allow_unicode=True).encode()


@runtime_checkable
class _VirtualHandler(Protocol):
    def accepts_path(self, path: str) -> bool: ...
    def merge(self, raw_indexes: list[bytes], member_names: list[str], member_configs: list[dict], proxy_base: str) -> bytes: ...
    def path_error(self) -> str: ...


class _HelmHandler:
    def accepts_path(self, path: str) -> bool:
        return path == "index.yaml"

    def merge(self, raw_indexes: list[bytes], member_names: list[str], member_configs: list[dict], proxy_base: str) -> bytes:
        return _merge_helm_indexes(raw_indexes, member_names, member_configs, proxy_base)

    def path_error(self) -> str:
        return "Virtual helm repositories only serve index.yaml; chart tarballs are served directly by member remotes"


_HANDLERS: dict[str, _VirtualHandler] = {
    "helm": _HelmHandler(),
}


async def handle(request: Request, virtual_name: str, path: str, storage, cache, config) -> Response:
    virtual_cfg = config.get_remote_config(virtual_name)
    if not virtual_cfg:
        raise HTTPException(status_code=404, detail=f"Virtual repository '{virtual_name}' not configured")
    if virtual_cfg.get("type") != "virtual":
        raise HTTPException(status_code=400, detail=f"'{virtual_name}' is not a virtual repository")

    package = virtual_cfg.get("package")
    handler = _HANDLERS.get(package)
    if handler is None:
        raise HTTPException(status_code=400, detail=f"Virtual repositories with package '{package}' are not yet supported")

    if not handler.accepts_path(path):
        raise HTTPException(status_code=404, detail=handler.path_error())

    members = virtual_cfg.get("members", [])
    if not members:
        raise HTTPException(status_code=500, detail=f"Virtual repository '{virtual_name}' has no members configured")

    virtual_key = storage.get_object_key(virtual_name, path)

    if cache.is_index_valid(virtual_name, path) and storage.exists(virtual_key):
        data = storage.download_object(virtual_key)
        logger.info(f"Virtual HIT: {virtual_name}/{path}")
        return Response(content=data, media_type="text/yaml")

    # Resolve configs first (config reads are sync/cheap)
    member_entries = []
    for member_name in members:
        member_cfg = config.get_remote_config(member_name)
        if not member_cfg:
            logger.warning(f"Virtual '{virtual_name}': member '{member_name}' not found in config, skipping")
            continue
        member_entries.append((member_name, member_cfg))

    # Fetch all member indexes in parallel; asyncio.gather preserves input order
    proxy_base = str(request.base_url).rstrip("/")
    t_fetch = time.perf_counter()
    results = await asyncio.gather(*[_get_member_index(name, cfg, path, storage, cache) for name, cfg in member_entries])
    fetch_ms = int((time.perf_counter() - t_fetch) * 1000)

    raw_indexes: list[bytes] = []
    used_members: list[str] = []
    used_configs: list[dict] = []
    min_ttl: int | None = None

    for member_name, member_cfg, member_ttl, raw_data in results:
        if min_ttl is None or member_ttl < min_ttl:
            min_ttl = member_ttl
        if raw_data is None:
            logger.warning(f"Virtual '{virtual_name}': skipping unreachable member '{member_name}'")
            continue
        raw_indexes.append(raw_data)
        used_members.append(member_name)
        used_configs.append(member_cfg)

    if not raw_indexes:
        raise HTTPException(status_code=502, detail=f"Virtual repository '{virtual_name}': no member indices could be fetched")

    if min_ttl is None:
        min_ttl = 3600

    t_merge = time.perf_counter()
    merged = handler.merge(raw_indexes, used_members, used_configs, proxy_base)
    merge_ms = int((time.perf_counter() - t_merge) * 1000)

    try:
        t_store = time.perf_counter()
        storage.upload(virtual_key, merged)
        cache.mark_index_cached(virtual_name, path, min_ttl)
        store_ms = int((time.perf_counter() - t_store) * 1000)
        logger.info(
            f"Virtual MISS: {virtual_name}/{path} rebuilt from {used_members} "
            f"(fetch={fetch_ms}ms merge={merge_ms}ms store={store_ms}ms ttl={min_ttl}s)"
        )
    except Exception as e:
        logger.warning(f"Virtual: failed to store merged index for '{virtual_name}': {e}")

    return Response(content=merged, media_type="text/yaml")
