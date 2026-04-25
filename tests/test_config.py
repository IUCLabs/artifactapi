"""Tests for ConfigManager, focusing on get_index_patterns (new logic)."""

import os

import pytest
import yaml

from artifactapi.config import ConfigManager


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
        patterns = cfg.get_index_patterns("r")
        # Assert against literal strings, not the live constant, so a rename doesn't mask a regression
        assert r"APKINDEX\.tar\.gz$" in patterns

    def test_rpm_returns_package_defaults(self, make_config):
        cfg = make_config({"r": {"type": "remote", "package": "rpm", "base_url": "https://x.com"}})
        patterns = cfg.get_index_patterns("r")
        assert r"repomd\.xml$" in patterns
        assert any("repodata" in p for p in patterns)

    def test_docker_returns_package_defaults(self, make_config):
        cfg = make_config({"r": {"type": "remote", "package": "docker", "base_url": "https://x.com"}})
        patterns = cfg.get_index_patterns("r")
        assert any("manifests" in p for p in patterns)
        assert any("tags/list" in p for p in patterns)

    def test_generic_returns_empty_list(self, make_config):
        cfg = make_config({"r": {"type": "remote", "package": "generic", "base_url": "https://x.com"}})
        assert cfg.get_index_patterns("r") == []

    def test_unknown_remote_returns_empty_list(self, make_config):
        cfg = make_config({})
        assert cfg.get_index_patterns("nonexistent") == []

    def test_missing_package_field_defaults_to_generic(self, make_config):
        cfg = make_config({"r": {"type": "remote", "base_url": "https://x.com"}})
        assert cfg.get_index_patterns("r") == []

    def test_unknown_package_type_returns_empty_list(self, make_config):
        # A mis-spelled package type silently returns [] — this is a known footgun
        cfg = make_config({"r": {"type": "remote", "package": "deb", "base_url": "https://x.com"}})
        assert cfg.get_index_patterns("r") == []

    def test_extra_patterns_appended_after_defaults(self, make_config):
        cfg = make_config(
            {
                "r": {
                    "type": "remote",
                    "package": "alpine",
                    "base_url": "https://x.com",
                    "index_patterns": [r"custom\.json$"],
                }
            }
        )
        patterns = cfg.get_index_patterns("r")
        assert r"APKINDEX\.tar\.gz$" in patterns
        assert r"custom\.json$" in patterns
        # Defaults come first
        assert patterns.index(r"APKINDEX\.tar\.gz$") < patterns.index(r"custom\.json$")

    def test_explicit_empty_extra_patterns_returns_defaults(self, make_config):
        cfg = make_config(
            {
                "r": {
                    "type": "remote",
                    "package": "alpine",
                    "base_url": "https://x.com",
                    "index_patterns": [],
                }
            }
        )
        assert r"APKINDEX\.tar\.gz$" in cfg.get_index_patterns("r")

    def test_duplicate_extra_pattern_not_added_twice(self, make_config):
        existing = r"APKINDEX\.tar\.gz$"
        cfg = make_config(
            {
                "r": {
                    "type": "remote",
                    "package": "alpine",
                    "base_url": "https://x.com",
                    "index_patterns": [existing],
                }
            }
        )
        patterns = cfg.get_index_patterns("r")
        assert patterns.count(existing) == 1

    def test_generic_with_only_extra_patterns(self, make_config):
        cfg = make_config(
            {
                "r": {
                    "type": "remote",
                    "package": "generic",
                    "base_url": "https://x.com",
                    "index_patterns": [r"meta\.json$", r"index\.yaml$"],
                }
            }
        )
        assert cfg.get_index_patterns("r") == [r"meta\.json$", r"index\.yaml$"]

    def test_rpm_extra_patterns_merged(self, make_config):
        cfg = make_config(
            {
                "r": {
                    "type": "remote",
                    "package": "rpm",
                    "base_url": "https://x.com",
                    "index_patterns": [r"custom-meta\.xml$"],
                }
            }
        )
        patterns = cfg.get_index_patterns("r")
        assert r"repomd\.xml$" in patterns
        assert r"custom-meta\.xml$" in patterns


# ---------------------------------------------------------------------------
# get_repository_patterns
# ---------------------------------------------------------------------------


class TestGetRepositoryPatterns:
    def test_returns_include_patterns(self, make_config):
        cfg = make_config(
            {
                "r": {
                    "type": "remote",
                    "package": "generic",
                    "base_url": "https://x.com",
                    "include_patterns": [r".*\.tar\.gz$"],
                }
            }
        )
        assert cfg.get_repository_patterns("r", "") == [r".*\.tar\.gz$"]

    def test_returns_empty_for_missing_remote(self, make_config):
        cfg = make_config({})
        assert cfg.get_repository_patterns("nonexistent", "") == []

    def test_returns_empty_when_no_patterns_configured(self, make_config):
        cfg = make_config({"r": {"type": "remote", "package": "generic", "base_url": "https://x.com"}})
        assert cfg.get_repository_patterns("r", "") == []

    def test_multiple_patterns_returned(self, make_config):
        patterns = [r".*\.rpm$", r".*/repodata/.*$"]
        cfg = make_config(
            {
                "r": {
                    "type": "remote",
                    "package": "rpm",
                    "base_url": "https://x.com",
                    "include_patterns": patterns,
                }
            }
        )
        assert cfg.get_repository_patterns("r", "") == patterns

    def test_dict_keyed_repositories_returns_per_repo_patterns(self, make_config):
        cfg = make_config(
            {
                "r": {
                    "type": "remote",
                    "package": "generic",
                    "base_url": "https://x.com",
                    "include_patterns": [r".*\.tar\.gz$"],
                    "repositories": {
                        "/path/to/repo": {"include_patterns": [r".*\.rpm$"]},
                    },
                }
            }
        )
        assert cfg.get_repository_patterns("r", "/path/to/repo") == [r".*\.rpm$"]

    def test_dict_keyed_repositories_falls_back_to_remote_patterns(self, make_config):
        cfg = make_config(
            {
                "r": {
                    "type": "remote",
                    "package": "generic",
                    "base_url": "https://x.com",
                    "include_patterns": [r".*\.tar\.gz$"],
                    "repositories": {
                        "/path/to/repo": {"include_patterns": [r".*\.rpm$"]},
                    },
                }
            }
        )
        assert cfg.get_repository_patterns("r", "/unknown/path") == [r".*\.tar\.gz$"]


# ---------------------------------------------------------------------------
# get_cache_config
# ---------------------------------------------------------------------------


class TestGetCacheConfig:
    def test_returns_cache_section(self, make_config):
        cfg = make_config(
            {
                "r": {
                    "type": "remote",
                    "package": "generic",
                    "base_url": "https://x.com",
                    "cache": {"file_ttl": 0, "index_ttl": 7200},
                }
            }
        )
        assert cfg.get_cache_config("r") == {"file_ttl": 0, "index_ttl": 7200}

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

        # Call check_reload without touching the file — should not reload
        cfg._check_reload()

        assert "repo-a" in cfg.config["remotes"]
