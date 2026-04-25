import time
import logging
import re
from typing import Optional
import httpx

logger = logging.getLogger(__name__)

# In-memory token cache: key -> (token, expires_at)
_token_cache: dict[str, tuple[str, float]] = {}

_WWW_AUTH_RE = re.compile(
    r'Bearer\s+realm="(?P<realm>[^"]+)"'
    r'(?:,service="(?P<service>[^"]*)")?'
    r'(?:,scope="(?P<scope>[^"]*)")?',
    re.IGNORECASE,
)


def _cache_key(realm: str, service: str, scope: str, username: Optional[str]) -> str:
    return f"{realm}|{service}|{scope}|{username or ''}"


def _get_cached_token(key: str) -> Optional[str]:
    entry = _token_cache.get(key)
    if entry and entry[1] > time.time():
        return entry[0]
    _token_cache.pop(key, None)
    return None


def _store_token(key: str, token: str, expires_in: int) -> None:
    # Expire 30s early to avoid using a token right as it expires
    _token_cache[key] = (token, time.time() + max(expires_in - 30, 10))


async def fetch_token(
    realm: str,
    service: str,
    scope: str,
    username: Optional[str] = None,
    password: Optional[str] = None,
) -> Optional[str]:
    """Fetch a Bearer token from a Docker registry auth server."""
    key = _cache_key(realm, service, scope, username)
    cached = _get_cached_token(key)
    if cached:
        return cached

    params: dict[str, str] = {}
    if service:
        params["service"] = service
    if scope:
        params["scope"] = scope

    auth = (username, password) if username and password else None

    try:
        async with httpx.AsyncClient(follow_redirects=True) as client:
            response = await client.get(realm, params=params, auth=auth)
            response.raise_for_status()
            data = response.json()
    except Exception as e:
        logger.warning(f"Docker token fetch failed ({realm}): {e}")
        return None

    token = data.get("token") or data.get("access_token")
    if not token:
        logger.warning(f"Docker token response missing token field: {data}")
        return None

    expires_in = int(data.get("expires_in", 300))
    _store_token(key, token, expires_in)
    logger.debug(f"Docker token obtained (realm={realm}, service={service}, scope={scope}, expires_in={expires_in}s)")
    return token


def parse_www_authenticate(header: str) -> Optional[tuple[str, str, str]]:
    """Parse WWW-Authenticate: Bearer header. Returns (realm, service, scope) or None."""
    m = _WWW_AUTH_RE.search(header)
    if not m:
        return None
    return m.group("realm"), m.group("service") or "", m.group("scope") or ""


async def get_docker_token_for_response(
    www_authenticate: str,
    username: Optional[str] = None,
    password: Optional[str] = None,
) -> Optional[str]:
    """Given a WWW-Authenticate header value, fetch and return a Bearer token."""
    parsed = parse_www_authenticate(www_authenticate)
    if not parsed:
        return None
    realm, service, scope = parsed
    return await fetch_token(realm, service, scope, username, password)
