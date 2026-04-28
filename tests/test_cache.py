"""Tests for RedisCache, focusing on is_mutable_file with configurable patterns."""

import hashlib
from unittest.mock import ANY, MagicMock, patch

import pytest

from artifactapi.cache import RedisCache
from artifactapi.config import _PACKAGE_MUTABLE_PATTERNS


@pytest.fixture
def bare_cache():
    """RedisCache instance bypassing __init__ (no Redis needed for pure-logic tests)."""
    return RedisCache.__new__(RedisCache)


@pytest.fixture
def unavailable_cache():
    """RedisCache where Redis is not reachable."""
    with patch("redis.from_url", side_effect=Exception("connection refused")):
        return RedisCache("redis://localhost:6379/0")


@pytest.fixture
def mock_redis_client():
    return MagicMock()


@pytest.fixture
def cache_with_redis(mock_redis_client):
    """RedisCache backed by a MagicMock Redis client."""
    with patch("redis.from_url", return_value=mock_redis_client):
        c = RedisCache("redis://localhost:6379/0")
    c.client = mock_redis_client
    c.available = True
    return c


# ---------------------------------------------------------------------------
# is_mutable_file — alpine patterns
# ---------------------------------------------------------------------------


class TestIsMutableFileAlpine:
    def test_apkindex_tarball_is_index(self, bare_cache):
        patterns = _PACKAGE_MUTABLE_PATTERNS["alpine"]
        assert bare_cache.is_mutable_file("alpine/v3.18/x86_64/APKINDEX.tar.gz", patterns)

    def test_nested_apkindex_is_index(self, bare_cache):
        patterns = _PACKAGE_MUTABLE_PATTERNS["alpine"]
        assert bare_cache.is_mutable_file("mirrors/dl-cdn/alpine/v3.19/community/x86_64/APKINDEX.tar.gz", patterns)

    def test_apk_package_is_not_index(self, bare_cache):
        patterns = _PACKAGE_MUTABLE_PATTERNS["alpine"]
        assert not bare_cache.is_mutable_file("alpine/v3.18/x86_64/musl-1.2.4-r2.apk", patterns)

    def test_random_tarball_is_not_index(self, bare_cache):
        patterns = _PACKAGE_MUTABLE_PATTERNS["alpine"]
        assert not bare_cache.is_mutable_file("some/path/archive.tar.gz", patterns)

    def test_apkindex_signature_file_is_not_index(self, bare_cache):
        # Signature file adjacent to the index should not be treated as an index
        patterns = _PACKAGE_MUTABLE_PATTERNS["alpine"]
        assert not bare_cache.is_mutable_file("alpine/v3.18/x86_64/APKINDEX.tar.gz.sig", patterns)

    def test_apkindex_tmp_file_is_not_index(self, bare_cache):
        patterns = _PACKAGE_MUTABLE_PATTERNS["alpine"]
        assert not bare_cache.is_mutable_file("alpine/v3.18/x86_64/APKINDEX.tar.gz.tmp", patterns)


# ---------------------------------------------------------------------------
# is_mutable_file — rpm patterns
# ---------------------------------------------------------------------------


class TestIsMutableFileRpm:
    def test_repomd_xml_is_index(self, bare_cache):
        patterns = _PACKAGE_MUTABLE_PATTERNS["rpm"]
        assert bare_cache.is_mutable_file("almalinux/9/x86_64/repomd.xml", patterns)

    def test_repodata_primary_xml_gz_is_index(self, bare_cache):
        patterns = _PACKAGE_MUTABLE_PATTERNS["rpm"]
        assert bare_cache.is_mutable_file("repo/repodata/primary.xml.gz", patterns)

    def test_repodata_sqlite_is_index(self, bare_cache):
        patterns = _PACKAGE_MUTABLE_PATTERNS["rpm"]
        assert bare_cache.is_mutable_file("repo/repodata/primary.sqlite", patterns)

    def test_repodata_sqlite_bz2_is_index(self, bare_cache):
        patterns = _PACKAGE_MUTABLE_PATTERNS["rpm"]
        assert bare_cache.is_mutable_file("repo/repodata/other.sqlite.bz2", patterns)

    def test_repodata_yaml_xz_is_index(self, bare_cache):
        patterns = _PACKAGE_MUTABLE_PATTERNS["rpm"]
        assert bare_cache.is_mutable_file("repo/repodata/comps.yaml.xz", patterns)

    def test_packages_gz_pattern_matches_any_path(self, bare_cache):
        # The Packages.gz$ regex is a carryover from the original hardcoded logic and
        # deliberately matches any path ending in Packages.gz — including Debian-style paths.
        # This test documents that intentional behaviour.
        patterns = _PACKAGE_MUTABLE_PATTERNS["rpm"]
        assert bare_cache.is_mutable_file("debian/dists/stable/main/binary-amd64/Packages.gz", patterns)

    def test_rpm_package_is_not_index(self, bare_cache):
        patterns = _PACKAGE_MUTABLE_PATTERNS["rpm"]
        assert not bare_cache.is_mutable_file("almalinux/9/x86_64/Packages/bash-5.1.8.x86_64.rpm", patterns)

    def test_arbitrary_xml_outside_repodata_is_not_index(self, bare_cache):
        patterns = _PACKAGE_MUTABLE_PATTERNS["rpm"]
        assert not bare_cache.is_mutable_file("some/path/config.xml", patterns)


# ---------------------------------------------------------------------------
# is_mutable_file — docker patterns
# ---------------------------------------------------------------------------


class TestIsMutableFileDocker:
    def test_tag_manifest_is_index(self, bare_cache):
        patterns = _PACKAGE_MUTABLE_PATTERNS["docker"]
        assert bare_cache.is_mutable_file("library/nginx/manifests/latest", patterns)

    def test_version_tag_manifest_is_index(self, bare_cache):
        patterns = _PACKAGE_MUTABLE_PATTERNS["docker"]
        assert bare_cache.is_mutable_file("library/nginx/manifests/1.25.3", patterns)

    def test_hyphenated_tag_manifest_is_index(self, bare_cache):
        patterns = _PACKAGE_MUTABLE_PATTERNS["docker"]
        assert bare_cache.is_mutable_file("library/nginx/manifests/latest-rc", patterns)

    def test_numeric_date_tag_manifest_is_index(self, bare_cache):
        patterns = _PACKAGE_MUTABLE_PATTERNS["docker"]
        assert bare_cache.is_mutable_file("library/nginx/manifests/20240101", patterns)

    def test_digest_manifest_is_not_index(self, bare_cache):
        patterns = _PACKAGE_MUTABLE_PATTERNS["docker"]
        digest = "sha256:" + "a" * 64
        assert not bare_cache.is_mutable_file(f"library/nginx/manifests/{digest}", patterns)

    def test_tags_list_is_index(self, bare_cache):
        patterns = _PACKAGE_MUTABLE_PATTERNS["docker"]
        assert bare_cache.is_mutable_file("library/nginx/tags/list", patterns)

    def test_blob_is_not_index(self, bare_cache):
        patterns = _PACKAGE_MUTABLE_PATTERNS["docker"]
        assert not bare_cache.is_mutable_file("library/nginx/blobs/sha256:abc123", patterns)


# ---------------------------------------------------------------------------
# is_mutable_file — edge cases
# ---------------------------------------------------------------------------


class TestIsMutableFileEdgeCases:
    def test_empty_patterns_nothing_is_index(self, bare_cache):
        assert not bare_cache.is_mutable_file("APKINDEX.tar.gz", [])
        assert not bare_cache.is_mutable_file("repomd.xml", [])
        assert not bare_cache.is_mutable_file("library/nginx/manifests/latest", [])

    def test_none_patterns_nothing_is_index(self, bare_cache):
        assert not bare_cache.is_mutable_file("APKINDEX.tar.gz", None)
        assert not bare_cache.is_mutable_file("repomd.xml", None)

    def test_custom_patterns_match(self, bare_cache):
        patterns = [r"metadata\.json$", r"index\.yaml$"]
        assert bare_cache.is_mutable_file("repo/metadata.json", patterns)
        assert bare_cache.is_mutable_file("repo/subdir/index.yaml", patterns)
        assert not bare_cache.is_mutable_file("repo/data.tar.gz", patterns)

    def test_custom_pattern_does_not_match_standard_index(self, bare_cache):
        patterns = [r"metadata\.json$"]
        assert not bare_cache.is_mutable_file("APKINDEX.tar.gz", patterns)


# ---------------------------------------------------------------------------
# get_index_cache_key
# ---------------------------------------------------------------------------


class TestGetIndexCacheKey:
    def test_key_format_is_deterministic(self, bare_cache):
        # Assert against a pre-computed value to pin the hash algorithm,
        # truncation length, and format string in one assertion.
        path = "alpine/v3.18/x86_64/APKINDEX.tar.gz"
        expected_hash = hashlib.sha256(path.encode()).hexdigest()[:16]
        key = bare_cache.get_index_cache_key("alpine-test", path)
        assert key == f"index:alpine-test:{expected_hash}"

    def test_different_paths_produce_different_keys(self, bare_cache):
        k1 = bare_cache.get_index_cache_key("alpine-test", "alpine/v3.18/x86_64/APKINDEX.tar.gz")
        k2 = bare_cache.get_index_cache_key("alpine-test", "alpine/v3.19/x86_64/APKINDEX.tar.gz")
        assert k1 != k2

    def test_different_remotes_produce_different_keys(self, bare_cache):
        k1 = bare_cache.get_index_cache_key("remote-a", "path/to/APKINDEX.tar.gz")
        k2 = bare_cache.get_index_cache_key("remote-b", "path/to/APKINDEX.tar.gz")
        assert k1 != k2

    def test_key_starts_with_index_prefix_and_remote(self, bare_cache):
        key = bare_cache.get_index_cache_key("myremote", "some/path")
        assert key.startswith("index:myremote:")

    def test_key_hash_segment_is_16_chars(self, bare_cache):
        key = bare_cache.get_index_cache_key("myremote", "some/path/file.xml")
        # Format: index:<remote>:<16-char hash> — the fixed length matters for key-space hygiene
        parts = key.split(":")
        assert len(parts) == 3
        assert len(parts[2]) == 16


# ---------------------------------------------------------------------------
# mark_index_cached / is_index_valid
# ---------------------------------------------------------------------------


class TestIndexValidity:
    def test_mark_index_cached_calls_setex_with_correct_ttl(self, cache_with_redis, mock_redis_client):
        cache_with_redis.mark_index_cached("remote", "path/APKINDEX.tar.gz", 300)
        expected_key = cache_with_redis.get_index_cache_key("remote", "path/APKINDEX.tar.gz")
        mock_redis_client.setex.assert_called_once_with(expected_key, 300, ANY)

    def test_present_key_is_valid(self, cache_with_redis, mock_redis_client):
        mock_redis_client.exists.return_value = 1
        assert cache_with_redis.is_index_valid("remote", "path/APKINDEX.tar.gz")

    def test_missing_key_is_not_valid(self, cache_with_redis, mock_redis_client):
        mock_redis_client.exists.return_value = 0
        assert not cache_with_redis.is_index_valid("remote", "path/APKINDEX.tar.gz")

    def test_unavailable_redis_is_not_valid(self, unavailable_cache):
        assert not unavailable_cache.is_index_valid("remote", "some/path")

    def test_mark_cached_no_op_when_unavailable(self, unavailable_cache):
        # client is None when Redis is unavailable — setex cannot be called
        assert unavailable_cache.client is None
        unavailable_cache.mark_index_cached("remote", "some/path", 300)  # must not raise


# ---------------------------------------------------------------------------
# mutable meta (ETag / Last-Modified storage)
# ---------------------------------------------------------------------------


class TestMutableMeta:
    def test_meta_key_format(self, bare_cache):
        path = "repo/metadata.json"
        expected_hash = hashlib.sha256(path.encode()).hexdigest()[:16]
        assert bare_cache.get_mutable_meta_key("myremote", path) == f"mutable:meta:myremote:{expected_hash}"

    def test_meta_key_hash_is_16_chars(self, bare_cache):
        key = bare_cache.get_mutable_meta_key("remote", "some/path/file.json")
        assert len(key.split(":")[-1]) == 16

    def test_store_and_retrieve_etag(self, cache_with_redis, mock_redis_client):
        mock_redis_client.hgetall.return_value = {"etag": '"abc123"'}
        cache_with_redis.store_mutable_meta("remote", "path/meta.json", '"abc123"', None)
        mock_redis_client.hset.assert_called_once()
        meta = cache_with_redis.get_mutable_meta("remote", "path/meta.json")
        assert meta["etag"] == '"abc123"'

    def test_store_and_retrieve_last_modified(self, cache_with_redis, mock_redis_client):
        lm = "Mon, 01 Jan 2024 00:00:00 GMT"
        mock_redis_client.hgetall.return_value = {"last_modified": lm}
        cache_with_redis.store_mutable_meta("remote", "path/meta.json", None, lm)
        meta = cache_with_redis.get_mutable_meta("remote", "path/meta.json")
        assert meta["last_modified"] == lm

    def test_store_no_op_when_both_none(self, cache_with_redis, mock_redis_client):
        cache_with_redis.store_mutable_meta("remote", "path/meta.json", None, None)
        mock_redis_client.hset.assert_not_called()

    def test_store_no_op_when_unavailable(self, unavailable_cache):
        unavailable_cache.store_mutable_meta("remote", "path", "etag", None)  # must not raise

    def test_get_returns_empty_when_unavailable(self, unavailable_cache):
        assert unavailable_cache.get_mutable_meta("remote", "path") == {}

    def test_delete_removes_meta_key(self, cache_with_redis, mock_redis_client):
        expected_key = cache_with_redis.get_mutable_meta_key("remote", "path/meta.json")
        cache_with_redis.delete_mutable_meta("remote", "path/meta.json")
        mock_redis_client.delete.assert_called_once_with(expected_key)

    def test_delete_no_op_when_unavailable(self, unavailable_cache):
        unavailable_cache.delete_mutable_meta("remote", "path")  # must not raise


# ---------------------------------------------------------------------------
# artifact published date (quarantine support)
# ---------------------------------------------------------------------------


class TestArtifactPublished:
    def test_key_format_is_deterministic(self, bare_cache):
        path = "some/path/package-1.0.tar.gz"
        expected_hash = hashlib.sha256(path.encode()).hexdigest()[:16]
        assert bare_cache.get_artifact_published_key("myremote", path) == f"pkg:published:myremote:{expected_hash}"

    def test_key_hash_is_16_chars(self, bare_cache):
        key = bare_cache.get_artifact_published_key("remote", "path/to/file.whl")
        assert len(key.split(":")[-1]) == 16

    def test_different_paths_produce_different_keys(self, bare_cache):
        k1 = bare_cache.get_artifact_published_key("remote", "pkg-1.0.tar.gz")
        k2 = bare_cache.get_artifact_published_key("remote", "pkg-2.0.tar.gz")
        assert k1 != k2

    def test_store_calls_set_with_correct_value(self, cache_with_redis, mock_redis_client):
        lm = "Mon, 01 Jan 2024 00:00:00 GMT"
        cache_with_redis.store_artifact_published("remote", "path/pkg.tar.gz", lm)
        expected_key = cache_with_redis.get_artifact_published_key("remote", "path/pkg.tar.gz")
        mock_redis_client.set.assert_called_once_with(expected_key, lm)

    def test_get_returns_stored_value(self, cache_with_redis, mock_redis_client):
        lm = "Tue, 15 Mar 2022 12:00:00 GMT"
        mock_redis_client.get.return_value = lm
        result = cache_with_redis.get_artifact_published("remote", "path/pkg.tar.gz")
        assert result == lm

    def test_get_returns_none_when_not_stored(self, cache_with_redis, mock_redis_client):
        mock_redis_client.get.return_value = None
        result = cache_with_redis.get_artifact_published("remote", "path/pkg.tar.gz")
        assert result is None

    def test_store_no_op_when_unavailable(self, unavailable_cache):
        unavailable_cache.store_artifact_published("remote", "path", "Mon, 01 Jan 2024 00:00:00 GMT")

    def test_get_returns_none_when_unavailable(self, unavailable_cache):
        assert unavailable_cache.get_artifact_published("remote", "path") is None
