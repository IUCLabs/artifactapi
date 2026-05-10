"""
Pytest configuration and shared fixtures.

Module-level setup (env vars + connection patches) runs before any test
module is imported, so the FastAPI app initialises against mocks rather
than real S3 / Redis / PostgreSQL services.
"""

import os
import tempfile
from unittest.mock import MagicMock, patch

import yaml

# ---------------------------------------------------------------------------
# Test remote configuration
# ---------------------------------------------------------------------------

TEST_REMOTES = {
    "remotes": {
        "alpine-test": {
            "base_url": "https://dl-cdn.alpinelinux.org",
            "package": "alpine",
            "immutable_patterns": [".*/x86_64/.*\\.apk$"],
            "cache": {"immutable_ttl": 0, "mutable_ttl": 3600},
        },
        "rpm-test": {
            "base_url": "https://example.com/rpm",
            "package": "rpm",
            "immutable_patterns": [".*/x86_64/.*\\.rpm$", ".*/repodata/.*$"],
            "cache": {"immutable_ttl": 0, "mutable_ttl": 3600},
        },
        "docker-test": {
            "base_url": "https://registry.example.com",
            "package": "docker",
            "cache": {"immutable_ttl": 0, "mutable_ttl": 300},
        },
        "docker-restricted": {
            "base_url": "https://registry.example.com",
            "package": "docker",
            "immutable_patterns": ["^library/nginx"],
            "cache": {"immutable_ttl": 0, "mutable_ttl": 300},
        },
        "docker-bantags-test": {
            "base_url": "https://registry.example.com",
            "package": "docker",
            "ban_tags_enabled": True,
            "ban_tags": ["latest", "edge"],
            "cache": {"immutable_ttl": 0, "mutable_ttl": 300},
        },
        "generic-test": {
            "base_url": "https://releases.example.com",
            "package": "generic",
            "immutable_patterns": [".*\\.tar\\.gz$"],
            "cache": {"immutable_ttl": 0, "mutable_ttl": 0},
        },
        "custom-index-test": {
            "base_url": "https://example.com",
            "package": "generic",
            "mutable_patterns": ["metadata\\.json$"],
            "cache": {"immutable_ttl": 0, "mutable_ttl": 600},
        },
        "check-mutable-test": {
            "base_url": "https://example.com",
            "package": "generic",
            "mutable_patterns": ["metadata\\.json$"],
            "check_mutable_updates": True,
            "cache": {"immutable_ttl": 0, "mutable_ttl": 600},
        },
        "pypi-test": {
            "base_url": "https://files.pythonhosted.org",
            "package": "pypi",
            "immutable_patterns": [
                r"packages/.*\.whl$",
                r"packages/.*\.whl\.metadata$",
                r"packages/.*\.tar\.gz$",
            ],
            "cache": {"immutable_ttl": 0, "mutable_ttl": 600},
        },
        "npm-test": {
            "base_url": "https://registry.npmjs.org",
            "package": "npm",
            "immutable_patterns": [r"\.tgz$"],
            "mutable_patterns": [r"^(?!.*\.tgz$).*"],
            "cache": {"immutable_ttl": 0, "mutable_ttl": 600},
        },
        "helm-test": {
            "base_url": "https://helm.releases.hashicorp.com",
            "package": "helm",
            "immutable_patterns": [r"\.tgz$"],
            "cache": {"immutable_ttl": 0, "mutable_ttl": 3600},
        },
        "quarantine-test": {
            "base_url": "https://releases.example.com",
            "package": "generic",
            "immutable_patterns": [r".*\.tar\.gz$"],
            "quarantine_new": True,
            "quarantine_days": 3,
            "cache": {"immutable_ttl": 0, "mutable_ttl": 0},
        },
        "quarantine-disabled": {
            "base_url": "https://releases.example.com",
            "package": "generic",
            "immutable_patterns": [r".*\.tar\.gz$"],
            "quarantine_new": False,
            "quarantine_days": 3,
            "cache": {"immutable_ttl": 0, "mutable_ttl": 0},
        },
        "helm-member-2": {
            "base_url": "https://charts.example.com",
            "package": "helm",
            "immutable_patterns": [r"\.tgz$"],
            "cache": {"immutable_ttl": 0, "mutable_ttl": 1800},
        },
    },
    "locals": {
        "local-test": {
            "package": "generic",
            "cache": {"immutable_ttl": 0, "mutable_ttl": 0},
        },
    },
    "virtuals": {
        "helm-virtual-test": {
            "package": "helm",
            "members": ["helm-test", "helm-member-2"],
        },
        "unsupported-virtual-test": {
            "package": "rpm",
            "members": ["rpm-test"],
        },
        "empty-virtual-test": {
            "package": "helm",
            "members": [],
        },
    },
}

# ---------------------------------------------------------------------------
# Write temp config and set env vars BEFORE importing the package
# ---------------------------------------------------------------------------

_tmpdir = tempfile.mkdtemp()
_config_path = os.path.join(_tmpdir, "remotes.yaml")
with open(_config_path, "w") as _f:
    yaml.dump(TEST_REMOTES, _f)

os.environ.update(
    {
        "CONFIG_PATH": _config_path,
        "MINIO_ENDPOINT": "localhost:9000",
        "MINIO_ACCESS_KEY": "testkey",
        "MINIO_SECRET_KEY": "testsecret",
        "MINIO_BUCKET": "testbucket",
        "REDIS_URL": "redis://localhost:6379/0",
        "DBHOST": "localhost",
        "DBPORT": "5432",
        "DBUSER": "test",
        "DBPASS": "test",
        "DBNAME": "test",
    }
)

# Patch external service connections before the package is imported.
# These stay active for the whole session (process exits after tests finish).
_boto3_patch = patch("boto3.client", return_value=MagicMock())
_redis_patch = patch("redis.from_url", return_value=MagicMock())
_psycopg2_patch = patch("psycopg2.connect", return_value=MagicMock())
_boto3_patch.start()
_redis_patch.start()
_psycopg2_patch.start()

# ---------------------------------------------------------------------------
# Shared fixtures
# ---------------------------------------------------------------------------

import pytest  # noqa: E402
from fastapi.testclient import TestClient  # noqa: E402


@pytest.fixture(scope="session")
def app():
    from artifactapi.main import app as fastapi_app

    return fastapi_app


@pytest.fixture(scope="session")
def client(app):
    return TestClient(app)


@pytest.fixture
def config_path():
    return _config_path


@pytest.fixture
def test_remotes():
    return TEST_REMOTES
