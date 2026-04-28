import re

from .base import get_content_type


def resolve_content(
    data: bytes,
    path: str,
    filename: str,
    immutable_patterns: list[str],
    base_url: str,
    proxy_url: str,
    remote_name: str,
) -> tuple[bytes, str]:
    if not any(re.search(p, path) for p in immutable_patterns):
        data = data.replace(
            base_url.encode(),
            f"{proxy_url}/api/v1/remote/{remote_name}".encode(),
        )
        return data, "application/json"
    return data, get_content_type(filename)
