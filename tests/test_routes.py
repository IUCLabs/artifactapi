"""FastAPI route tests using TestClient with mocked service dependencies."""
import hashlib
import json
from unittest.mock import AsyncMock, MagicMock, patch

import pytest

# ---------------------------------------------------------------------------
# Per-test service mocks (replace module-level globals in main.py)
# ---------------------------------------------------------------------------

@pytest.fixture
def mock_storage():
    m = MagicMock()
    m.get_object_key.return_value = "test-remote/abc123/file.ext"
    m.exists.return_value = False
    m.download_object.return_value = b"fake content"
    m.bucket = "testbucket"
    m.client = MagicMock()
    return m


@pytest.fixture
def mock_cache():
    m = MagicMock()
    m.is_index_file.return_value = False
    m.is_index_valid.return_value = True
    m.available = False
    m.client = None
    return m


@pytest.fixture
def mock_database():
    m = MagicMock()
    m.available = False
    return m


@pytest.fixture
def mock_metrics():
    return MagicMock()


@pytest.fixture
def patched_deps(mock_storage, mock_cache, mock_database, mock_metrics):
    """Swap the module-level service instances in main.py for the duration of a test."""
    import artifactapi.main as main_mod
    with (
        patch.object(main_mod, "storage", mock_storage),
        patch.object(main_mod, "cache", mock_cache),
        patch.object(main_mod, "database", mock_database),
        patch.object(main_mod, "metrics", mock_metrics),
    ):
        yield {
            "storage": mock_storage,
            "cache": mock_cache,
            "database": mock_database,
            "metrics": mock_metrics,
        }


# ---------------------------------------------------------------------------
# Basic / health endpoints
# ---------------------------------------------------------------------------

class TestBasicEndpoints:
    def test_root_returns_remote_list(self, client):
        response = client.get("/")
        assert response.status_code == 200
        data = response.json()
        assert "remotes" in data
        assert isinstance(data["remotes"], list)
        assert len(data["remotes"]) > 0

    def test_root_contains_version(self, client):
        response = client.get("/")
        assert "version" in response.json()

    def test_health_check(self, client):
        response = client.get("/health")
        assert response.status_code == 200
        assert response.json()["status"] == "healthy"

    def test_docker_v2_ping(self, client):
        response = client.get("/v2/")
        assert response.status_code == 200
        assert response.headers.get("Docker-Distribution-Api-Version") == "registry/2.0"
        assert response.json() == {}


# ---------------------------------------------------------------------------
# Docker proxy  /v2/{remote}/{path}
# ---------------------------------------------------------------------------

class TestDockerProxy:
    def test_unknown_remote_returns_404(self, client, patched_deps):
        response = client.get("/v2/no-such-remote/library/nginx/manifests/latest")
        assert response.status_code == 404

    def test_non_docker_package_returns_400(self, client, patched_deps):
        # alpine-test is package: alpine, not docker
        response = client.get("/v2/alpine-test/library/nginx/manifests/latest")
        assert response.status_code == 400

    def test_pattern_blocked_returns_403(self, client, patched_deps):
        # docker-restricted allows only "library/nginx"
        response = client.get("/v2/docker-restricted/library/ubuntu/manifests/latest")
        assert response.status_code == 403

    def test_allowed_pattern_proceeds_to_cache(self, client, patched_deps):
        deps = patched_deps
        manifest = json.dumps({
            "mediaType": "application/vnd.oci.image.manifest.v1+json",
            "layers": [],
        }).encode()
        deps["storage"].exists.return_value = True
        deps["storage"].download_object.return_value = manifest
        deps["cache"].is_index_file.return_value = True
        deps["cache"].is_index_valid.return_value = True

        response = client.get("/v2/docker-restricted/library/nginx/manifests/latest")
        assert response.status_code == 200

    def test_cache_hit_manifest_returns_correct_content_type(self, client, patched_deps):
        deps = patched_deps
        manifest = json.dumps({
            "mediaType": "application/vnd.docker.distribution.manifest.v2+json",
            "schemaVersion": 2,
            "layers": [],
        }).encode()
        deps["storage"].exists.return_value = True
        deps["storage"].download_object.return_value = manifest
        deps["cache"].is_index_file.return_value = True
        deps["cache"].is_index_valid.return_value = True

        response = client.get("/v2/docker-test/library/nginx/manifests/latest")
        assert response.status_code == 200
        ct = response.headers["content-type"]
        assert ct.startswith("application/vnd.docker.distribution.manifest.v2+json")

    def test_cache_hit_sets_docker_content_digest_header(self, client, patched_deps):
        deps = patched_deps
        manifest = json.dumps({
            "mediaType": "application/vnd.oci.image.manifest.v1+json",
            "layers": [],
        }).encode()
        deps["storage"].exists.return_value = True
        deps["storage"].download_object.return_value = manifest
        deps["cache"].is_index_file.return_value = True
        deps["cache"].is_index_valid.return_value = True

        response = client.get("/v2/docker-test/library/nginx/manifests/latest")
        expected = f"sha256:{hashlib.sha256(manifest).hexdigest()}"
        assert response.headers["Docker-Content-Digest"] == expected

    def test_head_request_returns_no_body(self, client, patched_deps):
        deps = patched_deps
        manifest = json.dumps({
            "mediaType": "application/vnd.oci.image.manifest.v1+json",
            "layers": [],
        }).encode()
        deps["storage"].exists.return_value = True
        deps["storage"].download_object.return_value = manifest
        deps["cache"].is_index_file.return_value = False

        response = client.head("/v2/docker-test/library/nginx/manifests/latest")
        assert response.status_code == 200
        assert response.content == b""

    def test_cache_miss_calls_upstream_fetch(self, client, patched_deps):
        deps = patched_deps
        manifest = json.dumps({
            "mediaType": "application/vnd.oci.image.manifest.v1+json",
            "layers": [],
        }).encode()
        deps["storage"].exists.return_value = False
        deps["storage"].download_object.return_value = manifest
        deps["cache"].is_index_file.return_value = True

        with patch(
            "artifactapi.main.cache_single_artifact",
            new_callable=AsyncMock,
            return_value={"status": "cached"},
        ) as mock_fetch:
            response = client.get("/v2/docker-test/library/nginx/manifests/latest")

        mock_fetch.assert_called_once()
        assert response.status_code == 200

    def test_index_expired_triggers_refetch(self, client, patched_deps):
        deps = patched_deps
        manifest = json.dumps({
            "mediaType": "application/vnd.oci.image.manifest.v1+json",
            "layers": [],
        }).encode()
        deps["storage"].exists.return_value = True   # cached in S3
        deps["cache"].is_index_file.return_value = True
        deps["cache"].is_index_valid.return_value = False  # but TTL expired
        deps["storage"].download_object.return_value = manifest

        with patch(
            "artifactapi.main.cache_single_artifact",
            new_callable=AsyncMock,
            return_value={"status": "cached"},
        ) as mock_fetch:
            response = client.get("/v2/docker-test/library/nginx/manifests/latest")

        mock_fetch.assert_called_once()
        assert response.status_code == 200


# ---------------------------------------------------------------------------
# Generic artifact route  /api/v1/remote/{remote}/{path}
# ---------------------------------------------------------------------------

class TestGenericArtifactRoute:
    def test_unknown_remote_returns_404(self, client, patched_deps):
        response = client.get("/api/v1/remote/nonexistent/path/to/file.tar.gz")
        assert response.status_code == 404

    def test_pattern_blocked_returns_403(self, client, patched_deps):
        # generic-test only allows .tar.gz
        response = client.get("/api/v1/remote/generic-test/some/path/file.rpm")
        assert response.status_code == 403

    def test_cache_hit_returns_200_with_header(self, client, patched_deps):
        deps = patched_deps
        deps["storage"].exists.return_value = True
        deps["storage"].download_object.return_value = b"tar content"
        deps["cache"].is_index_file.return_value = False

        response = client.get("/api/v1/remote/generic-test/some/path/archive.tar.gz")
        assert response.status_code == 200
        assert response.headers["X-Artifact-Source"] == "cache"
        assert response.content == b"tar content"

    def test_cache_miss_fetches_upstream_and_returns_200(self, client, patched_deps):
        deps = patched_deps
        deps["storage"].exists.return_value = False
        deps["storage"].download_object.return_value = b"fresh content"
        deps["cache"].is_index_file.return_value = False

        with patch(
            "artifactapi.main.cache_single_artifact",
            new_callable=AsyncMock,
            return_value={"status": "cached"},
        ) as mock_fetch:
            response = client.get("/api/v1/remote/generic-test/some/path/archive.tar.gz")

        mock_fetch.assert_called_once()
        assert response.status_code == 200
        assert response.headers["X-Artifact-Source"] == "remote"

    def test_upstream_error_returns_502(self, client, patched_deps):
        deps = patched_deps
        deps["storage"].exists.return_value = False
        deps["cache"].is_index_file.return_value = False

        with patch(
            "artifactapi.main.cache_single_artifact",
            new_callable=AsyncMock,
            return_value={"status": "error", "error": "upstream unreachable"},
        ):
            response = client.get("/api/v1/remote/generic-test/some/path/archive.tar.gz")

        assert response.status_code == 502

    def test_index_file_bypasses_include_patterns(self, client, patched_deps):
        """Index files must be served even when they don't match include_patterns."""
        deps = patched_deps
        deps["storage"].exists.return_value = True
        deps["storage"].download_object.return_value = b"APKINDEX content"
        # Simulate is_index_file returning True (APKINDEX.tar.gz detected as index)
        deps["cache"].is_index_file.return_value = True
        deps["cache"].is_index_valid.return_value = True

        # APKINDEX.tar.gz does not match alpine-test's include_patterns (.*.apk$),
        # but since it's an index file it must be allowed through.
        response = client.get("/api/v1/remote/alpine-test/alpine/v3.18/x86_64/APKINDEX.tar.gz")
        assert response.status_code == 200

    def test_local_repo_file_not_found_returns_404(self, client, patched_deps):
        deps = patched_deps
        deps["database"].get_local_file_metadata.return_value = None
        deps["database"].available = True

        response = client.get("/api/v1/remote/local-test/path/to/nonexistent.bin")
        assert response.status_code == 404


# ---------------------------------------------------------------------------
# Cache flush  PUT /cache/flush
# ---------------------------------------------------------------------------

class TestCacheFlushEndpoint:
    def test_flush_all_returns_flushed_structure(self, client, patched_deps):
        deps = patched_deps
        deps["cache"].available = False
        deps["storage"].client.list_objects_v2.return_value = {}

        response = client.put("/cache/flush")
        assert response.status_code == 200
        data = response.json()
        assert "flushed" in data
        assert "redis_keys" in data["flushed"]
        assert "s3_objects" in data["flushed"]

    def test_flush_specific_remote_echoes_remote(self, client, patched_deps):
        deps = patched_deps
        deps["cache"].available = False
        deps["storage"].client.list_objects_v2.return_value = {}

        response = client.put("/cache/flush?remote=alpine-test")
        assert response.status_code == 200
        assert response.json()["remote"] == "alpine-test"
