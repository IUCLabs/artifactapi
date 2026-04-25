"""Tests for S3Storage: get_object_key (pure logic) and I/O methods."""

import hashlib
from unittest.mock import MagicMock, patch

import pytest
from botocore.exceptions import ClientError
from fastapi import HTTPException

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
    def test_key_has_three_part_structure(self, storage):
        # remote / hash-segment / filename
        key = storage.get_object_key("myremote", "some/path/to/file.rpm")
        parts = key.split("/")
        assert len(parts) == 3
        assert parts[0] == "myremote"
        assert parts[2] == "file.rpm"
        assert len(parts[1]) == 16  # SHA-256 hex truncated to 16 chars

    def test_key_uses_sha256_of_directory_path(self, storage):
        # Pin the hash algorithm, truncation length, and format in one assertion
        key = storage.get_object_key("myremote", "some/path/to/file.rpm")
        expected_hash = hashlib.sha256(b"some/path/to").hexdigest()[:16]
        assert key == f"myremote/{expected_hash}/file.rpm"

    def test_different_remotes_give_different_keys(self, storage):
        k1 = storage.get_object_key("remote-a", "path/to/file.rpm")
        k2 = storage.get_object_key("remote-b", "path/to/file.rpm")
        assert k1 != k2

    def test_different_directories_give_different_keys(self, storage):
        k1 = storage.get_object_key("myremote", "path/version-1/file.rpm")
        k2 = storage.get_object_key("myremote", "path/version-2/file.rpm")
        assert k1 != k2
        assert k1.split("/")[-1] == k2.split("/")[-1] == "file.rpm"

    def test_leading_slash_stripped(self, storage):
        k1 = storage.get_object_key("myremote", "/path/to/file.rpm")
        k2 = storage.get_object_key("myremote", "path/to/file.rpm")
        assert k1 == k2

    def test_file_with_no_directory(self, storage):
        key = storage.get_object_key("myremote", "file.rpm")
        assert key == "myremote/file.rpm"

    def test_docker_blob_uses_digest_path(self, storage):
        digest = "a" * 64  # realistic 64-char SHA-256 hex string
        path = f"library/nginx/blobs/sha256:{digest}"
        key = storage.get_object_key("dockerhub", path)
        assert key == f"dockerhub/blobs/sha256/{digest}"

    def test_docker_blob_deduplication_across_images(self, storage):
        """Same blob digest pulled from different images maps to the same S3 key."""
        digest = "deadbeef" * 8  # 64-char hex
        k1 = storage.get_object_key("dockerhub", f"library/nginx/blobs/sha256:{digest}")
        k2 = storage.get_object_key("dockerhub", f"library/ubuntu/blobs/sha256:{digest}")
        assert k1 == k2

    def test_docker_blob_different_digests_different_keys(self, storage):
        k1 = storage.get_object_key("dockerhub", "library/nginx/blobs/sha256:" + "a" * 64)
        k2 = storage.get_object_key("dockerhub", "library/nginx/blobs/sha256:" + "b" * 64)
        assert k1 != k2

    def test_docker_blob_different_remotes_different_keys(self, storage):
        digest = "abc" * 21 + "d"  # 64-char hex
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

    def test_returns_http_url_for_secure_storage(self):
        with patch("boto3.client", return_value=MagicMock()):
            s = S3Storage(endpoint="s3.example.com", access_key="k", secret_key="s", bucket="b", secure=True)
        s.client = MagicMock()
        # get_url uses http:// always (direct internal access address, not the S3 protocol)
        assert s.get_url("path/to/file.rpm") == "http://s3.example.com/b/path/to/file.rpm"


# ---------------------------------------------------------------------------
# upload / download_object
# ---------------------------------------------------------------------------


class TestUpload:
    def test_upload_returns_s3_uri(self, storage):
        storage.client.put_object.return_value = {}
        result = storage.upload("myremote/abc123/file.rpm", b"content")
        assert result == "s3://testbucket/myremote/abc123/file.rpm"


class TestDownloadObject:
    def test_download_object_raises_404_on_client_error(self, storage):
        storage.client.get_object.side_effect = ClientError(
            {"Error": {"Code": "NoSuchKey", "Message": "The specified key does not exist"}},
            "GetObject",
        )
        with pytest.raises(HTTPException) as exc_info:
            storage.download_object("nonexistent/key")
        assert exc_info.value.status_code == 404
