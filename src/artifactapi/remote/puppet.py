from .base import get_content_type


def resolve_content(
    data: bytes,
    path: str,
    filename: str,
    base_url: str,
    proxy_url: str,
    remote_name: str,
) -> tuple[bytes, str]:
    if not path.startswith("v3/files/"):
        proxy_remote_url = f"{proxy_url}/api/v1/remote/{remote_name}"
        # Rewrite any absolute forge API URLs
        data = data.replace(base_url.encode(), proxy_remote_url.encode())
        # Rewrite relative file_uri paths ("/v3/files/...") to absolute proxy URLs.
        # g10k resolves file_uri against only the forge host, so a relative path
        # would drop our /api/v1/remote/<name> prefix.
        data = data.replace(
            b'"/v3/files/',
            f'"{proxy_remote_url}/v3/files/'.encode(),
        )
        return data, "application/json"
    return data, get_content_type(filename)
