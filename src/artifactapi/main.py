import logging
import os

from fastapi import FastAPI, File, Query, Request, UploadFile
from fastapi.responses import PlainTextResponse
from prometheus_client import CONTENT_TYPE_LATEST, generate_latest
from pydantic import BaseModel

try:
    from importlib.metadata import version

    __version__ = version("artifactapi")
except ImportError:
    __version__ = "dev"

from .artifact import discovery, flush, local, proxy, virtual
from .artifact import docker as docker_handler
from .cache import RedisCache
from .config import ConfigManager
from .database import DatabaseManager
from .metrics import MetricsManager
from .storage import S3Storage

logging.basicConfig(level=logging.INFO, format="%(asctime)s - %(name)s - %(levelname)s - %(message)s")
logger = logging.getLogger(__name__)

app = FastAPI(title="Artifact Storage API", version=__version__)

config_path = os.environ.get("CONFIG_PATH")
if not config_path:
    raise ValueError("CONFIG_PATH environment variable is required")
config = ConfigManager(config_path)

s3_config = config.get_s3_config()
redis_config = config.get_redis_config()
db_config = config.get_database_config()

storage = S3Storage(**s3_config)
cache = RedisCache(redis_config["url"])
database = DatabaseManager(db_config["url"])
metrics = MetricsManager(cache, database)


class ArtifactRequest(BaseModel):
    remote: str
    include_pattern: str


@app.get("/")
def read_root():
    config._check_reload()
    return {
        "message": "Artifact Storage API",
        "version": app.version,
        "remotes": list(config.config.get("remotes", {}).keys()),
        "virtuals": list(config.config.get("virtuals", {}).keys()),
        "locals": list(config.config.get("locals", {}).keys()),
    }


@app.get("/health")
def health_check():
    return {"status": "healthy"}


@app.get("/config")
def get_config():
    return config.config


@app.get("/metrics")
def get_metrics(json: bool | None = Query(False, description="Return JSON format instead of Prometheus")):
    config._check_reload()
    if json:
        return metrics.get_metrics(storage, config)
    metrics.get_metrics(storage, config)
    return PlainTextResponse(generate_latest().decode("utf-8"), media_type=CONTENT_TYPE_LATEST)


@app.put("/cache/flush")
def flush_cache(
    remote: str = Query(default=None, description="Specific remote to flush (optional)"),
    cache_type: str = Query(default="all", description="Type to flush: 'all', 'index', 'files', 'metrics'"),
):
    return flush.handle(remote, cache_type, cache, storage)


@app.get("/v2/")
async def docker_v2_ping():
    return docker_handler.ping()


@app.api_route("/v2/{remote_name}/{path:path}", methods=["GET", "HEAD"])
async def docker_v2_proxy(request: Request, remote_name: str, path: str):
    return await docker_handler.proxy(request, remote_name, path, storage, cache, config, metrics)


@app.get("/api/v1/virtual/{virtual_name}/{path:path}")
async def get_virtual_artifact(request: Request, virtual_name: str, path: str):
    return await virtual.handle(request, virtual_name, path, storage, cache, config)


@app.get("/api/v1/remote/{remote_name}/{path:path}")
async def get_artifact(request: Request, remote_name: str, path: str):
    return await proxy.handle(request, remote_name, path, storage, cache, config, database, metrics)


@app.get("/api/v1/local/{local_name}/{path:path}")
def get_local_artifact(local_name: str, path: str):
    return local.download(local_name, path, storage, database, config)


@app.put("/api/v1/local/{local_name}/{path:path}")
async def upload_local_file(local_name: str, path: str, file: UploadFile = File(...)):
    return await local.upload(local_name, path, file, storage, database, config)


@app.head("/api/v1/local/{local_name}/{path:path}")
def check_local_file_exists(local_name: str, path: str):
    return local.check_exists(local_name, path, database, config)


@app.delete("/api/v1/local/{local_name}/{path:path}")
def delete_local_file(local_name: str, path: str):
    return local.delete(local_name, path, storage, database, config)


@app.post("/api/v1/artifacts/cache")
async def cache_artifact(request: ArtifactRequest):
    return await discovery.cache_artifacts(request.remote, request.include_pattern, storage)


@app.get("/api/v1/artifacts/{remote:path}")
async def list_cached_artifacts(remote: str, include_pattern: str = ".*"):
    return await discovery.list_artifacts(remote, include_pattern, storage)


def main():
    import uvicorn

    uvicorn.run(app, host="0.0.0.0", port=8000)


if __name__ == "__main__":
    main()
