def get_content_type(filename: str) -> str:
    if filename.endswith((".tar.gz", ".tgz")):
        return "application/gzip"
    if filename.endswith(".zip") or filename.endswith(".whl"):
        return "application/zip"
    if filename.endswith(".exe"):
        return "application/x-msdownload"
    if filename.endswith(".rpm"):
        return "application/x-rpm"
    if filename.endswith(".xml"):
        return "application/xml"
    if filename.endswith((".xml.gz", ".xml.bz2", ".xml.xz")):
        return "application/gzip"
    if filename.endswith((".yaml", ".yml")):
        return "text/yaml"
    return "application/octet-stream"
