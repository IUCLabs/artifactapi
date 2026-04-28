import re

from .base import get_content_type


def construct_url(base_url: str, path: str) -> str:
    """Build the upstream URL for a PyPI request.

    PyPI splits simple/ index pages (pypi.org) from file downloads
    (files.pythonhosted.org), so simple/ requests are redirected to pypi.org.
    """
    if base_url.rstrip("/") == "https://files.pythonhosted.org" and "simple/" in path:
        return f"https://pypi.org/{path}"
    return f"{base_url}/{path}"


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
        return data, "text/html; charset=utf-8"
    return data, get_content_type(filename)
