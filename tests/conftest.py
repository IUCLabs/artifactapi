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
            "type": "remote",
            "package": "alpine",
            "immutable_patterns": [".*/x86_64/.*\\.apk$"],
            "cache": {"immutable_ttl": 0, "mutable_ttl": 3600},
        },
        "rpm-test": {
            "base_url": "https://example.com/rpm",
            "type": "remote",
            "package": "rpm",
            "immutable_patterns": [".*/x86_64/.*\\.rpm$", ".*/repodata/.*$"],
            "cache": {"immutable_ttl": 0, "mutable_ttl": 3600},
        },
        "docker-test": {
            "base_url": "https://registry.example.com",
            "type": "remote",
            "package": "docker",
            "cache": {"immutable_ttl": 0, "mutable_ttl": 300},
        },
        "docker-restricted": {
            "base_url": "https://registry.example.com",
            "type": "remote",
            "package": "docker",
            "immutable_patterns": ["^library/nginx"],
            "cache": {"immutable_ttl": 0, "mutable_ttl": 300},
        },
        "generic-test": {
            "base_url": "https://releases.example.com",
            "type": "remote",
            "package": "generic",
            "immutable_patterns": [".*\\.tar\\.gz$"],
            "cache": {"immutable_ttl": 0, "mutable_ttl": 0},
        },
        "custom-index-test": {
            "base_url": "https://example.com",
            "type": "remote",
            "package": "generic",
            "mutable_patterns": ["metadata\\.json$"],
            "cache": {"immutable_ttl": 0, "mutable_ttl": 600},
        },
        "check-mutable-test": {
            "base_url": "https://example.com",
            "type": "remote",
            "package": "generic",
            "mutable_patterns": ["metadata\\.json$"],
            "check_mutable_updates": True,
            "cache": {"immutable_ttl": 0, "mutable_ttl": 600},
        },
        "local-test": {
            "type": "local",
            "package": "generic",
            "cache": {"immutable_ttl": 0, "mutable_ttl": 0},
        },
        "pypi-test": {
            "base_url": "https://files.pythonhosted.org",
            "type": "remote",
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
            "type": "remote",
            "package": "npm",
            "immutable_patterns": [r"\.tgz$"],
            "mutable_patterns": [r"^(?!.*\.tgz$).*"],
            "cache": {"immutable_ttl": 0, "mutable_ttl": 600},
        },
        "helm-test": {
            "base_url": "https://helm.releases.hashicorp.com",
            "type": "remote",
            "package": "helm",
            "immutable_patterns": [r"\.tgz$"],
            "cache": {"immutable_ttl": 0, "mutable_ttl": 3600},
        },
    }
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
