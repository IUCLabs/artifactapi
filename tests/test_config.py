"""Tests for ConfigManager, focusing on get_index_patterns (new logic)."""
import os

import pytest
import yaml

from artifactapi.config import _PACKAGE_INDEX_PATTERNS, ConfigManager


@pytest.fixture
def make_config(tmp_path):
    """Factory: write a remotes dict to a temp YAML and return a ConfigManager."""
    def _make(remotes_dict):
        cfg_file = tmp_path / "remotes.yaml"
        cfg_file.write_text(yaml.dump({"remotes": remotes_dict}))
        return ConfigManager(str(cfg_file))
    return _make


# ---------------------------------------------------------------------------
# get_index_patterns
# ---------------------------------------------------------------------------

class TestGetIndexPatterns:
    def test_alpine_returns_package_defaults(self, make_config):
        cfg = make_config({"r": {"type": "remote", "package": "alpine", "base_url": "https://x.com"}})
        assert cfg.get_index_patterns("r") == _PACKAGE_INDEX_PATTERNS["alpine"]

    def test_rpm_returns_package_defaults(self, make_config):
        cfg = make_config({"r": {"type": "remote", "package": "rpm", "base_url": "https://x.com"}})
        assert cfg.get_index_patterns("r") == _PACKAGE_INDEX_PATTERNS["rpm"]

    def test_docker_returns_package_defaults(self, make_config):
        cfg = make_config({"r": {"type": "remote", "package": "docker", "base_url": "https://x.com"}})
        assert cfg.get_index_patterns("r") == _PACKAGE_INDEX_PATTERNS["docker"]

    def test_generic_returns_empty_list(self, make_config):
        cfg = make_config({"r": {"type": "remote", "package": "generic", "base_url": "https://x.com"}})
        assert cfg.get_index_patterns("r") == []

    def test_unknown_remote_returns_empty_list(self, make_config):
        cfg = make_config({})
        assert cfg.get_index_patterns("nonexistent") == []

    def test_missing_package_field_defaults_to_generic(self, make_config):
        cfg = make_config({"r": {"type": "remote", "base_url": "https://x.com"}})
        assert cfg.get_index_patterns("r") == []

    def test_extra_patterns_appended_after_defaults(self, make_config):
        cfg = make_config({
            "r": {
                "type": "remote",
                "package": "alpine",
                "base_url": "https://x.com",
                "index_patterns": ["custom\\.json$"],
            }
        })
        patterns = cfg.get_index_patterns("r")
        defaults = _PACKAGE_INDEX_PATTERNS["alpine"]
        assert patterns[: len(defaults)] == defaults
        assert "custom\\.json$" in patterns

    def test_duplicate_extra_pattern_not_added_twice(self, make_config):
        existing = _PACKAGE_INDEX_PATTERNS["alpine"][0]
        cfg = make_config({
            "r": {
                "type": "remote",
                "package": "alpine",
                "base_url": "https://x.com",
                "index_patterns": [existing],
            }
        })
        patterns = cfg.get_index_patterns("r")
        assert patterns.count(existing) == 1

    def test_generic_with_only_extra_patterns(self, make_config):
        cfg = make_config({
            "r": {
                "type": "remote",
                "package": "generic",
                "base_url": "https://x.com",
                "index_patterns": ["meta\\.json$", "index\\.yaml$"],
            }
        })
        assert cfg.get_index_patterns("r") == ["meta\\.json$", "index\\.yaml$"]

    def test_rpm_extra_patterns_merged(self, make_config):
        cfg = make_config({
            "r": {
                "type": "remote",
                "package": "rpm",
                "base_url": "https://x.com",
                "index_patterns": ["custom-meta\\.xml$"],
            }
        })
        patterns = cfg.get_index_patterns("r")
        for default in _PACKAGE_INDEX_PATTERNS["rpm"]:
            assert default in patterns
        assert "custom-meta\\.xml$" in patterns


# ---------------------------------------------------------------------------
# get_repository_patterns
# ---------------------------------------------------------------------------

class TestGetRepositoryPatterns:
    def test_returns_include_patterns(self, make_config):
        cfg = make_config({
            "r": {
                "type": "remote",
                "package": "generic",
                "base_url": "https://x.com",
                "include_patterns": [".*\\.tar\\.gz$"],
            }
        })
        assert cfg.get_repository_patterns("r", "") == [".*\\.tar\\.gz$"]

    def test_returns_empty_for_missing_remote(self, make_config):
        cfg = make_config({})
        assert cfg.get_repository_patterns("nonexistent", "") == []

    def test_returns_empty_when_no_patterns_configured(self, make_config):
        cfg = make_config({"r": {"type": "remote", "package": "generic", "base_url": "https://x.com"}})
        assert cfg.get_repository_patterns("r", "") == []

    def test_multiple_patterns_returned(self, make_config):
        patterns = [".*\\.rpm$", ".*/repodata/.*$"]
        cfg = make_config({
            "r": {
                "type": "remote",
                "package": "rpm",
                "base_url": "https://x.com",
                "include_patterns": patterns,
            }
        })
        assert cfg.get_repository_patterns("r", "") == patterns


# ---------------------------------------------------------------------------
# get_cache_config
# ---------------------------------------------------------------------------

class TestGetCacheConfig:
    def test_returns_cache_section(self, make_config):
        cfg = make_config({
            "r": {
                "type": "remote",
                "package": "generic",
                "base_url": "https://x.com",
                "cache": {"file_ttl": 0, "index_ttl": 7200},
            }
        })
        result = cfg.get_cache_config("r")
        assert result["index_ttl"] == 7200
        assert result["file_ttl"] == 0

    def test_returns_empty_dict_for_missing_remote(self, make_config):
        cfg = make_config({})
        assert cfg.get_cache_config("nonexistent") == {}

    def test_returns_empty_dict_when_no_cache_key(self, make_config):
        cfg = make_config({"r": {"type": "remote", "package": "generic", "base_url": "https://x.com"}})
        assert cfg.get_cache_config("r") == {}


# ---------------------------------------------------------------------------
# Config file reload
# ---------------------------------------------------------------------------

class TestConfigReload:
    def test_reloads_when_file_mtime_advances(self, tmp_path):
        cfg_file = tmp_path / "remotes.yaml"
        cfg_file.write_text(yaml.dump({"remotes": {"repo-a": {"type": "remote", "package": "generic", "base_url": "https://x.com"}}}))
        cfg = ConfigManager(str(cfg_file))
        assert "repo-a" in cfg.config["remotes"]

        cfg_file.write_text(yaml.dump({"remotes": {"repo-b": {"type": "remote", "package": "generic", "base_url": "https://y.com"}}}))
        future_mtime = cfg._last_modified + 1
        os.utime(str(cfg_file), (future_mtime, future_mtime))

        cfg._check_reload()

        assert "repo-b" in cfg.config["remotes"]
        assert "repo-a" not in cfg.config["remotes"]

    def test_no_reload_when_file_unchanged(self, tmp_path):
        cfg_file = tmp_path / "remotes.yaml"
        cfg_file.write_text(yaml.dump({"remotes": {"repo-a": {"type": "remote", "package": "generic", "base_url": "https://x.com"}}}))
        cfg = ConfigManager(str(cfg_file))

        # Call check_reload without touching the file → should not reload
        cfg._check_reload()

        assert "repo-a" in cfg.config["remotes"]
