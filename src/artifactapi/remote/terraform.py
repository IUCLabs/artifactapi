import json
import re
from urllib.parse import urlparse

from .base import get_content_type

_DOWNLOAD_PATH = re.compile(r"^[^/]+/[^/]+/[^/]+/download/[^/]+/[^/]+$")


def construct_url(base_url: str, path: str) -> str:
    return f"{base_url}/v1/providers/{path}"


def resolve_content(
    data: bytes,
    path: str,
    filename: str,
    _base_url: str,
    proxy_url: str,
    _remote_name: str,
    releases_remote: str | None = None,
) -> tuple[bytes, str]:
    if filename.endswith((".zip", ".sig")):
        return data, get_content_type(filename)
    if releases_remote and _DOWNLOAD_PATH.match(path):
        releases_proxy = f"{proxy_url}/api/v1/remote/{releases_remote}"
        try:
            obj = json.loads(data)
            for field in ("download_url", "shasums_url", "shasums_signature_url"):
                if field in obj:
                    parsed = urlparse(obj[field])
                    obj[field] = f"{releases_proxy}{parsed.path}"
            data = json.dumps(obj).encode()
        except (json.JSONDecodeError, KeyError):
            pass
    return data, "application/json"
