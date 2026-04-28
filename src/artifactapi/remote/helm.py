from .base import get_content_type


def resolve_content(
    data: bytes,
    path: str,
    filename: str,
    base_url: str,
    proxy_url: str,
    remote_name: str,
) -> tuple[bytes, str]:
    if filename == "index.yaml":
        data = data.replace(
            base_url.encode(),
            f"{proxy_url}/api/v1/remote/{remote_name}".encode(),
        )
        return data, "text/yaml"
    return data, get_content_type(filename)
