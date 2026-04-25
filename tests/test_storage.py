"""Tests for S3Storage, focusing on get_object_key (pure logic, no S3 calls)."""
from unittest.mock import MagicMock, patch

import pytest

from artifactapi.storage import S3Storage


@pytest.fixture
def storage():
    """S3Storage with a mocked boto3 client."""
    with patch("boto3.client", return_value=MagicMock()):
        s = S3Storage(
            endpoint="localhost:9000",
            access_key="testkey",
            secret_key="testsecret",
            bucket="testbucket",
            secure=False,
        )
    s.client = MagicMock()
    return s


# ---------------------------------------------------------------------------
# get_object_key
# ---------------------------------------------------------------------------

class TestGetObjectKey:
    def test_includes_remote_name_prefix(self, storage):
        key = storage.get_object_key("myremote", "some/path/file.rpm")
        assert key.startswith("myremote/")

    def test_ends_with_filename(self, storage):
        key = storage.get_object_key("myremote", "some/path/file.rpm")
        assert key.endswith("/file.rpm")

    def test_same_path_produces_same_key(self, storage):
        k1 = storage.get_object_key("myremote", "some/path/file.rpm")
        k2 = storage.get_object_key("myremote", "some/path/file.rpm")
        assert k1 == k2

    def test_different_remotes_give_different_keys(self, storage):
        k1 = storage.get_object_key("remote-a", "path/to/file.rpm")
        k2 = storage.get_object_key("remote-b", "path/to/file.rpm")
        assert k1 != k2

    def test_different_directories_give_different_keys(self, storage):
        k1 = storage.get_object_key("myremote", "path/version-1/file.rpm")
        k2 = storage.get_object_key("myremote", "path/version-2/file.rpm")
        assert k1 != k2
        # Same filename, different directory hashes
        assert k1.split("/")[-1] == k2.split("/")[-1] == "file.rpm"

    def test_leading_slash_stripped(self, storage):
        k1 = storage.get_object_key("myremote", "/path/to/file.rpm")
        k2 = storage.get_object_key("myremote", "path/to/file.rpm")
        assert k1 == k2

    def test_file_with_no_directory(self, storage):
        key = storage.get_object_key("myremote", "file.rpm")
        assert key == "myremote/file.rpm"

    def test_docker_blob_uses_digest_path(self, storage):
        digest = "abc123def456" * 4
        path = f"library/nginx/blobs/sha256:{digest}"
        key = storage.get_object_key("dockerhub", path)
        assert key == f"dockerhub/blobs/sha256/{digest}"

    def test_docker_blob_deduplication_across_images(self, storage):
        """Same blob digest pulled from different images maps to the same S3 key."""
        digest = "deadbeef" * 8
        k1 = storage.get_object_key("dockerhub", f"library/nginx/blobs/sha256:{digest}")
        k2 = storage.get_object_key("dockerhub", f"library/ubuntu/blobs/sha256:{digest}")
        assert k1 == k2

    def test_docker_blob_different_digests_different_keys(self, storage):
        k1 = storage.get_object_key("dockerhub", "library/nginx/blobs/sha256:aaa111")
        k2 = storage.get_object_key("dockerhub", "library/nginx/blobs/sha256:bbb222")
        assert k1 != k2

    def test_docker_blob_different_remotes_different_keys(self, storage):
        digest = "abc" * 20
        k1 = storage.get_object_key("remote-a", f"library/nginx/blobs/sha256:{digest}")
        k2 = storage.get_object_key("remote-b", f"library/nginx/blobs/sha256:{digest}")
        assert k1 != k2


# ---------------------------------------------------------------------------
# get_url
# ---------------------------------------------------------------------------

class TestGetUrl:
    def test_returns_http_url_for_insecure_endpoint(self, storage):
        url = storage.get_url("myremote/abc123/file.rpm")
        assert url == "http://localhost:9000/testbucket/myremote/abc123/file.rpm"

    def test_url_contains_bucket_and_key(self, storage):
        key = "myremote/abc/file.tar.gz"
        url = storage.get_url(key)
        assert "testbucket" in url
        assert key in url
