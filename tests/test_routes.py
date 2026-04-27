"""FastAPI route tests using TestClient with mocked service dependencies."""

import hashlib
import json
from unittest.mock import ANY, AsyncMock, MagicMock, patch

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
    m.is_mutable_file.return_value = False
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
        manifest = json.dumps(
            {
                "mediaType": "application/vnd.oci.image.manifest.v1+json",
                "layers": [],
            }
        ).encode()
        deps["storage"].exists.return_value = True
        deps["storage"].download_object.return_value = manifest
        deps["cache"].is_mutable_file.return_value = True
        deps["cache"].is_index_valid.return_value = True

        response = client.get("/v2/docker-restricted/library/nginx/manifests/latest")
        assert response.status_code == 200

    def test_cache_hit_manifest_returns_correct_content_type(self, client, patched_deps):
        deps = patched_deps
        manifest = json.dumps(
            {
                "mediaType": "application/vnd.docker.distribution.manifest.v2+json",
                "schemaVersion": 2,
                "layers": [],
            }
        ).encode()
        deps["storage"].exists.return_value = True
        deps["storage"].download_object.return_value = manifest
        deps["cache"].is_mutable_file.return_value = True
        deps["cache"].is_index_valid.return_value = True

        response = client.get("/v2/docker-test/library/nginx/manifests/latest")
        assert response.status_code == 200
        ct = response.headers["content-type"]
        assert ct.startswith("application/vnd.docker.distribution.manifest.v2+json")

    def test_cache_hit_sets_docker_content_digest_header(self, client, patched_deps):
        deps = patched_deps
        manifest = json.dumps(
            {
                "mediaType": "application/vnd.oci.image.manifest.v1+json",
                "layers": [],
            }
        ).encode()
        deps["storage"].exists.return_value = True
        deps["storage"].download_object.return_value = manifest
        deps["cache"].is_mutable_file.return_value = True
        deps["cache"].is_index_valid.return_value = True

        response = client.get("/v2/docker-test/library/nginx/manifests/latest")
        expected = f"sha256:{hashlib.sha256(manifest).hexdigest()}"
        assert response.headers["Docker-Content-Digest"] == expected

    def test_cache_hit_records_metrics(self, client, patched_deps):
        deps = patched_deps
        manifest = json.dumps({"mediaType": "application/vnd.oci.image.manifest.v1+json", "layers": []}).encode()
        deps["storage"].exists.return_value = True
        deps["storage"].download_object.return_value = manifest
        deps["cache"].is_mutable_file.return_value = False

        client.get("/v2/docker-test/library/nginx/manifests/latest")
        deps["metrics"].record_cache_hit.assert_called_once_with("docker-test", ANY)

    def test_head_request_returns_no_body(self, client, patched_deps):
        deps = patched_deps
        manifest = json.dumps(
            {
                "mediaType": "application/vnd.oci.image.manifest.v1+json",
                "layers": [],
            }
        ).encode()
        deps["storage"].exists.return_value = True
        deps["storage"].download_object.return_value = manifest
        deps["cache"].is_mutable_file.return_value = False

        response = client.head("/v2/docker-test/library/nginx/manifests/latest")
        assert response.status_code == 200
        assert response.content == b""

    def test_cache_miss_calls_upstream_fetch(self, client, patched_deps):
        deps = patched_deps
        manifest = json.dumps(
            {
                "mediaType": "application/vnd.oci.image.manifest.v1+json",
                "layers": [],
            }
        ).encode()
        deps["storage"].exists.return_value = False
        deps["storage"].download_object.return_value = manifest
        deps["cache"].is_mutable_file.return_value = True

        with patch(
            "artifactapi.main.cache_single_artifact",
            new_callable=AsyncMock,
            return_value={"status": "cached"},
        ) as mock_fetch:
            response = client.get("/v2/docker-test/library/nginx/manifests/latest")

        mock_fetch.assert_called_once()
        assert response.status_code == 200

    def test_cache_miss_on_index_marks_index_cached(self, client, patched_deps):
        deps = patched_deps
        manifest = json.dumps(
            {
                "mediaType": "application/vnd.oci.image.manifest.v1+json",
                "layers": [],
            }
        ).encode()
        deps["storage"].exists.return_value = False
        deps["storage"].download_object.return_value = manifest
        deps["cache"].is_mutable_file.return_value = True

        with patch(
            "artifactapi.main.cache_single_artifact",
            new_callable=AsyncMock,
            return_value={"status": "cached"},
        ):
            response = client.get("/v2/docker-test/library/nginx/manifests/latest")

        assert response.status_code == 200
        deps["cache"].mark_index_cached.assert_called_once()

    def test_index_expired_triggers_refetch(self, client, patched_deps):
        deps = patched_deps
        manifest = json.dumps(
            {
                "mediaType": "application/vnd.oci.image.manifest.v1+json",
                "layers": [],
            }
        ).encode()
        deps["storage"].exists.return_value = True  # cached in S3
        deps["cache"].is_mutable_file.return_value = True
        deps["cache"].is_index_valid.return_value = False  # but TTL expired
        deps["storage"].download_object.return_value = manifest

        with patch("artifactapi.main._upstream_reachable", new_callable=AsyncMock, return_value=True):
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

    def test_cache_hit_returns_200_with_source_header(self, client, patched_deps):
        deps = patched_deps
        deps["storage"].exists.return_value = True
        deps["storage"].download_object.return_value = b"tar content"
        deps["cache"].is_mutable_file.return_value = False

        response = client.get("/api/v1/remote/generic-test/some/path/archive.tar.gz")
        assert response.status_code == 200
        assert response.headers["X-Artifact-Source"] == "cache"
        assert response.content == b"tar content"

    def test_cache_hit_sets_content_disposition(self, client, patched_deps):
        deps = patched_deps
        deps["storage"].exists.return_value = True
        deps["storage"].download_object.return_value = b"content"
        deps["cache"].is_mutable_file.return_value = False

        response = client.get("/api/v1/remote/generic-test/some/path/archive.tar.gz")
        disposition = response.headers["content-disposition"]
        assert "attachment" in disposition
        assert "archive.tar.gz" in disposition

    def test_cache_hit_sets_artifact_size_header(self, client, patched_deps):
        deps = patched_deps
        content = b"some artifact content bytes"
        deps["storage"].exists.return_value = True
        deps["storage"].download_object.return_value = content
        deps["cache"].is_mutable_file.return_value = False

        response = client.get("/api/v1/remote/generic-test/some/path/archive.tar.gz")
        assert response.headers["X-Artifact-Size"] == str(len(content))

    def test_cache_hit_records_metrics(self, client, patched_deps):
        deps = patched_deps
        deps["storage"].exists.return_value = True
        deps["storage"].download_object.return_value = b"content"
        deps["cache"].is_mutable_file.return_value = False

        client.get("/api/v1/remote/generic-test/some/path/archive.tar.gz")
        deps["metrics"].record_cache_hit.assert_called_once_with("generic-test", ANY)

    def test_cache_hit_records_artifact_mapping(self, client, patched_deps):
        deps = patched_deps
        deps["storage"].exists.return_value = True
        deps["storage"].download_object.return_value = b"content"
        deps["cache"].is_mutable_file.return_value = False

        client.get("/api/v1/remote/generic-test/some/path/archive.tar.gz")
        deps["database"].record_artifact_mapping.assert_called_once()

    def test_cache_hit_rpm_returns_correct_content_type(self, client, patched_deps):
        deps = patched_deps
        deps["storage"].exists.return_value = True
        deps["storage"].download_object.return_value = b"rpm bytes"
        deps["cache"].is_mutable_file.return_value = False

        response = client.get("/api/v1/remote/rpm-test/almalinux/9/x86_64/bash-5.1.8.x86_64.rpm")
        assert response.status_code == 200
        assert "application/x-rpm" in response.headers["content-type"]

    def test_cache_hit_xml_returns_correct_content_type(self, client, patched_deps):
        deps = patched_deps
        deps["storage"].exists.return_value = True
        deps["storage"].download_object.return_value = b"<?xml version='1.0'?>"
        deps["cache"].is_mutable_file.return_value = False

        response = client.get("/api/v1/remote/rpm-test/repo/repodata/primary.xml")
        assert response.status_code == 200
        assert "application/xml" in response.headers["content-type"]

    def test_cache_miss_fetches_upstream_and_returns_200(self, client, patched_deps):
        deps = patched_deps
        deps["storage"].exists.return_value = False
        deps["storage"].download_object.return_value = b"fresh content"
        deps["cache"].is_mutable_file.return_value = False

        with patch(
            "artifactapi.main.cache_single_artifact",
            new_callable=AsyncMock,
            return_value={"status": "cached"},
        ) as mock_fetch:
            response = client.get("/api/v1/remote/generic-test/some/path/archive.tar.gz")

        mock_fetch.assert_called_once()
        assert response.status_code == 200
        assert response.headers["X-Artifact-Source"] == "remote"

    def test_cache_miss_records_metrics(self, client, patched_deps):
        deps = patched_deps
        deps["storage"].exists.return_value = False
        deps["storage"].download_object.return_value = b"fresh content"
        deps["cache"].is_mutable_file.return_value = False

        with patch(
            "artifactapi.main.cache_single_artifact",
            new_callable=AsyncMock,
            return_value={"status": "cached"},
        ):
            client.get("/api/v1/remote/generic-test/some/path/archive.tar.gz")

        deps["metrics"].record_cache_miss.assert_called_once_with("generic-test", ANY)

    def test_cache_miss_on_index_marks_index_cached(self, client, patched_deps):
        deps = patched_deps
        deps["storage"].exists.return_value = False
        deps["storage"].download_object.return_value = b"APKINDEX content"
        deps["cache"].is_mutable_file.return_value = True

        with patch(
            "artifactapi.main.cache_single_artifact",
            new_callable=AsyncMock,
            return_value={"status": "cached"},
        ):
            response = client.get("/api/v1/remote/alpine-test/alpine/v3.18/x86_64/APKINDEX.tar.gz")

        assert response.status_code == 200
        deps["cache"].mark_index_cached.assert_called_once()

    def test_upstream_error_returns_502(self, client, patched_deps):
        deps = patched_deps
        deps["storage"].exists.return_value = False
        deps["cache"].is_mutable_file.return_value = False

        with patch(
            "artifactapi.main.cache_single_artifact",
            new_callable=AsyncMock,
            return_value={"status": "error", "error": "upstream unreachable"},
        ):
            response = client.get("/api/v1/remote/generic-test/some/path/archive.tar.gz")

        assert response.status_code == 502

    def test_mutable_file_bypasses_immutable_patterns(self, client, patched_deps):
        """Mutable files must be served even when they don't match immutable_patterns."""
        deps = patched_deps
        deps["storage"].exists.return_value = True
        deps["storage"].download_object.return_value = b"APKINDEX content"
        deps["cache"].is_mutable_file.return_value = True
        deps["cache"].is_index_valid.return_value = True

        # APKINDEX.tar.gz does not match alpine-test's immutable_patterns (.*.apk$),
        # but since is_mutable_file returns True it must be allowed through.
        response = client.get("/api/v1/remote/alpine-test/alpine/v3.18/x86_64/APKINDEX.tar.gz")
        assert response.status_code == 200

    def test_mutable_unchanged_refreshes_ttl_without_redownload(self, client, patched_deps):
        """When check_mutable_updates=True and upstream says 304, TTL is refreshed in place."""
        deps = patched_deps
        deps["storage"].exists.return_value = True
        deps["storage"].download_object.return_value = b"metadata content"
        # File is mutable and its TTL has expired
        deps["cache"].is_mutable_file.return_value = True
        deps["cache"].is_index_valid.return_value = False
        deps["cache"].get_mutable_meta.return_value = {"etag": '"abc"'}

        with patch("artifactapi.main.check_upstream_changed", new_callable=AsyncMock, return_value=False):
            response = client.get("/api/v1/remote/check-mutable-test/metadata.json")

        assert response.status_code == 200
        deps["cache"].mark_index_cached.assert_called()
        # S3 object must NOT have been deleted (no re-download)
        deps["storage"].client.delete_object.assert_not_called()

    def test_mutable_changed_triggers_redownload(self, client, patched_deps):
        """When check_mutable_updates=True and upstream says 200, cache is invalidated."""
        deps = patched_deps
        deps["storage"].exists.return_value = False
        deps["cache"].is_mutable_file.return_value = True
        deps["cache"].is_index_valid.return_value = False
        deps["cache"].get_mutable_meta.return_value = {"etag": '"abc"'}

        with patch("artifactapi.main.check_upstream_changed", new_callable=AsyncMock, return_value=True):
            with patch("artifactapi.main.cache_single_artifact", new_callable=AsyncMock) as mock_cache:
                mock_cache.return_value = {"status": "error", "error": "upstream gone"}
                response = client.get("/api/v1/remote/check-mutable-test/metadata.json")

        assert response.status_code == 502

    def test_mutable_changed_redownloads_successfully(self, client, patched_deps):
        """When check_mutable_updates=True and upstream says 200, fresh copy is fetched and served."""
        deps = patched_deps
        deps["storage"].exists.return_value = True
        deps["storage"].download_object.return_value = b"fresh metadata"
        deps["cache"].is_mutable_file.return_value = True
        deps["cache"].is_index_valid.return_value = False
        deps["cache"].get_mutable_meta.return_value = {"etag": '"abc"'}

        with patch("artifactapi.main.check_upstream_changed", new_callable=AsyncMock, return_value=True):
            with patch("artifactapi.main.cache_single_artifact", new_callable=AsyncMock) as mock_cache:
                mock_cache.return_value = {"status": "cached", "etag": '"def"', "last_modified": None}
                response = client.get("/api/v1/remote/check-mutable-test/metadata.json")

        assert response.status_code == 200
        mock_cache.assert_called_once()

    def test_mutable_backend_unreachable_on_check_updates_keeps_stale(self, client, patched_deps):
        """When check_mutable_updates=True and backend is unreachable, stale copy is kept and TTL refreshed."""
        from artifactapi.main import UpstreamUnreachable

        deps = patched_deps
        deps["storage"].exists.return_value = True
        deps["storage"].download_object.return_value = b"stale metadata"
        deps["cache"].is_mutable_file.return_value = True
        deps["cache"].is_index_valid.return_value = False
        deps["cache"].get_mutable_meta.return_value = {"etag": '"abc"'}

        with patch("artifactapi.main.check_upstream_changed", side_effect=UpstreamUnreachable("connection refused")):
            response = client.get("/api/v1/remote/check-mutable-test/metadata.json")

        assert response.status_code == 200
        deps["cache"].mark_index_cached.assert_called()
        deps["storage"].client.delete_object.assert_not_called()

    def test_mutable_backend_unreachable_on_expiry_keeps_stale(self, client, patched_deps):
        """When a regular mutable file expires and backend is unreachable, stale copy is kept and TTL refreshed."""
        deps = patched_deps
        deps["storage"].exists.return_value = True
        deps["storage"].download_object.return_value = b"stale APKINDEX"
        deps["cache"].is_mutable_file.return_value = True
        deps["cache"].is_index_valid.return_value = False

        with patch("artifactapi.main._upstream_reachable", new_callable=AsyncMock, return_value=False):
            response = client.get("/api/v1/remote/alpine-test/alpine/v3.18/x86_64/APKINDEX.tar.gz")

        assert response.status_code == 200
        deps["cache"].mark_index_cached.assert_called()
        deps["storage"].client.delete_object.assert_not_called()

    def test_mutable_flag_off_skips_conditional_check(self, client, patched_deps):
        """When check_mutable_updates is not set, expired mutable files are always re-fetched."""
        deps = patched_deps
        deps["storage"].exists.return_value = False
        deps["cache"].is_mutable_file.return_value = True
        deps["cache"].is_index_valid.return_value = False

        with patch("artifactapi.main.check_upstream_changed", new_callable=AsyncMock) as mock_check:
            with patch("artifactapi.main.cache_single_artifact", new_callable=AsyncMock) as mock_cache:
                mock_cache.return_value = {"status": "error", "error": "upstream gone"}
                client.get("/api/v1/remote/custom-index-test/metadata.json")

        mock_check.assert_not_called()

    def test_local_repo_file_not_found_returns_404(self, client, patched_deps):
        deps = patched_deps
        deps["database"].get_local_file_metadata.return_value = None
        deps["database"].available = True

        response = client.get("/api/v1/remote/local-test/path/to/nonexistent.bin")
        assert response.status_code == 404


# ---------------------------------------------------------------------------
# Upload route  PUT /api/v1/remote/{remote}/{path}
# ---------------------------------------------------------------------------


class TestUploadRoute:
    def test_unknown_remote_returns_404(self, client, patched_deps):
        response = client.put(
            "/api/v1/remote/nonexistent/path/to/file.tar.gz",
            files={"file": ("file.tar.gz", b"content", "application/octet-stream")},
        )
        assert response.status_code == 404

    def test_non_local_remote_returns_400(self, client, patched_deps):
        response = client.put(
            "/api/v1/remote/generic-test/path/to/file.tar.gz",
            files={"file": ("file.tar.gz", b"content", "application/octet-stream")},
        )
        assert response.status_code == 400


# ---------------------------------------------------------------------------
# HEAD route  HEAD /api/v1/remote/{remote}/{path}
# ---------------------------------------------------------------------------


class TestHeadRoute:
    def test_non_local_remote_returns_405(self, client, patched_deps):
        response = client.head("/api/v1/remote/generic-test/path/to/file.tar.gz")
        assert response.status_code == 405

    def test_local_repo_file_not_found_returns_404(self, client, patched_deps):
        deps = patched_deps
        deps["database"].get_local_file_metadata.return_value = None
        deps["database"].available = True

        response = client.head("/api/v1/remote/local-test/path/to/nonexistent.bin")
        assert response.status_code == 404

    def test_unknown_remote_returns_404(self, client, patched_deps):
        response = client.head("/api/v1/remote/nonexistent/path/to/file.bin")
        assert response.status_code == 404


# ---------------------------------------------------------------------------
# DELETE route  DELETE /api/v1/remote/{remote}/{path}
# ---------------------------------------------------------------------------


class TestDeleteRoute:
    def test_unknown_remote_returns_404(self, client, patched_deps):
        response = client.delete("/api/v1/remote/nonexistent/path/to/file.tar.gz")
        assert response.status_code == 404

    def test_non_local_remote_returns_400(self, client, patched_deps):
        response = client.delete("/api/v1/remote/generic-test/path/to/file.tar.gz")
        assert response.status_code == 400


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

    def test_flush_all_deletes_redis_keys_when_cache_available(self, client, patched_deps):
        deps = patched_deps
        deps["cache"].available = True
        redis_mock = MagicMock()
        deps["cache"].client = redis_mock
        # index:* returns keys; mutable:meta:* and metrics:* return nothing
        redis_mock.keys.side_effect = [["index:test:abc", "index:test:def"], [], []]
        deps["storage"].client.list_objects_v2.return_value = {}

        response = client.put("/cache/flush")
        assert response.status_code == 200
        data = response.json()
        assert data["flushed"]["redis_keys"] == 2
        redis_mock.delete.assert_called_once_with("index:test:abc", "index:test:def")


# ---------------------------------------------------------------------------
# Metrics endpoint  GET /metrics
# ---------------------------------------------------------------------------


class TestMetricsEndpoint:
    def test_returns_prometheus_text_by_default(self, client, patched_deps):
        response = client.get("/metrics")
        assert response.status_code == 200
        assert response.headers["content-type"].startswith("text/plain")


# ---------------------------------------------------------------------------
# Config endpoint  GET /config
# ---------------------------------------------------------------------------


class TestConfigEndpoint:
    def test_returns_config_with_remotes(self, client):
        response = client.get("/config")
        assert response.status_code == 200
        data = response.json()
        assert "remotes" in data
        assert "alpine-test" in data["remotes"]
