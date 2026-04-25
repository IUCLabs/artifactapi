"""Tests for docker_auth: WWW-Authenticate parsing and token caching."""

import time
from unittest.mock import AsyncMock, MagicMock, patch

import httpx
import pytest

from artifactapi import docker_auth
from artifactapi.docker_auth import (
    _cache_key,
    _get_cached_token,
    _store_token,
    fetch_token,
    get_docker_token_for_response,
    parse_www_authenticate,
)


@pytest.fixture(autouse=True)
def clear_token_cache():
    """Isolate tests: wipe the module-level token cache before and after each test."""
    docker_auth._token_cache.clear()
    yield
    docker_auth._token_cache.clear()


# ---------------------------------------------------------------------------
# parse_www_authenticate
# ---------------------------------------------------------------------------


class TestParseWwwAuthenticate:
    def test_full_bearer_header(self):
        header = 'Bearer realm="https://auth.docker.io/token",service="registry.docker.io",scope="repository:library/nginx:pull"'
        result = parse_www_authenticate(header)
        assert result is not None
        realm, service, scope = result
        assert realm == "https://auth.docker.io/token"
        assert service == "registry.docker.io"
        assert scope == "repository:library/nginx:pull"

    def test_realm_only(self):
        header = 'Bearer realm="https://auth.example.com/token"'
        result = parse_www_authenticate(header)
        assert result is not None
        realm, service, scope = result
        assert realm == "https://auth.example.com/token"
        assert service == ""
        assert scope == ""

    def test_realm_and_service_only(self):
        header = 'Bearer realm="https://auth.example.com",service="registry.example.com"'
        result = parse_www_authenticate(header)
        assert result is not None
        _, service, scope = result
        assert service == "registry.example.com"
        assert scope == ""

    def test_invalid_scheme_returns_none(self):
        assert parse_www_authenticate('Basic realm="example"') is None

    def test_empty_header_returns_none(self):
        assert parse_www_authenticate("") is None

    def test_case_insensitive_bearer_parses_realm(self):
        header = 'bearer realm="https://auth.example.com/token"'
        result = parse_www_authenticate(header)
        assert result is not None
        realm, _, _ = result
        assert realm == "https://auth.example.com/token"

    def test_field_order_scope_before_service_drops_service(self):
        # The regex requires realm,service,scope order; scope before service
        # results in service being silently dropped. This test documents the known limitation.
        header = 'Bearer realm="https://auth.example.com",scope="repo:pull",service="svc"'
        result = parse_www_authenticate(header)
        assert result is not None
        realm, service, scope = result
        assert realm == "https://auth.example.com"
        assert scope == "repo:pull"
        assert service == ""  # silently dropped when out of order


# ---------------------------------------------------------------------------
# _cache_key
# ---------------------------------------------------------------------------


class TestCacheKey:
    def test_key_contains_all_components(self):
        key = _cache_key("https://realm.com", "svc", "scope", "user")
        assert "https://realm.com" in key
        assert "svc" in key
        assert "scope" in key
        assert "user" in key

    def test_none_username_uses_empty_string(self):
        key = _cache_key("https://realm.com", "svc", "scope", None)
        assert key.endswith("|")

    def test_different_services_give_different_keys(self):
        k1 = _cache_key("realm", "svc1", "scope", None)
        k2 = _cache_key("realm", "svc2", "scope", None)
        assert k1 != k2

    def test_different_scopes_give_different_keys(self):
        k1 = _cache_key("realm", "svc", "scope:read", None)
        k2 = _cache_key("realm", "svc", "scope:write", None)
        assert k1 != k2

    def test_pipe_in_field_value_can_collide_with_adjacent_fields(self):
        # The "|" separator is not escaped, so a pipe embedded in one field
        # produces the same key as the same pipe appearing as a separator boundary.
        # This is a known limitation: _cache_key("a|b","c","d",None) ==
        # _cache_key("a","b|c","d",None). Documents the behaviour, not a claim it's correct.
        k1 = _cache_key("a|b", "c", "d", None)
        k2 = _cache_key("a", "b|c", "d", None)
        assert k1 == k2


# ---------------------------------------------------------------------------
# _get_cached_token / _store_token
# ---------------------------------------------------------------------------


class TestTokenCaching:
    def test_get_returns_none_when_not_cached(self):
        assert _get_cached_token("no-such-key") is None

    def test_get_returns_token_when_valid(self):
        _store_token("mykey", "tok-abc", 300)
        assert _get_cached_token("mykey") == "tok-abc"

    def test_get_returns_none_when_expired(self):
        docker_auth._token_cache["mykey"] = ("old-token", time.time() - 1)
        assert _get_cached_token("mykey") is None

    def test_expired_entry_is_removed_from_cache(self):
        docker_auth._token_cache["mykey"] = ("old-token", time.time() - 1)
        _get_cached_token("mykey")
        assert "mykey" not in docker_auth._token_cache

    def test_store_expires_30s_before_stated_time(self):
        before = time.time()
        _store_token("mykey", "tok", 100)
        _, expires_at = docker_auth._token_cache["mykey"]
        # expires_in - 30 = 70; allow ±2 s clock wiggle
        assert before + 68 <= expires_at <= before + 72

    def test_store_enforces_minimum_10s_expiry(self):
        before = time.time()
        _store_token("mykey", "tok", 5)  # expires_in - 30 would be negative
        _, expires_at = docker_auth._token_cache["mykey"]
        assert expires_at >= before + 10


# ---------------------------------------------------------------------------
# fetch_token (async, mocks httpx)
# ---------------------------------------------------------------------------


def _make_mock_http_client(token_payload: dict):
    mock_response = MagicMock()
    mock_response.raise_for_status = MagicMock()
    mock_response.json.return_value = token_payload

    mock_client = AsyncMock()
    mock_client.get = AsyncMock(return_value=mock_response)

    ctx = MagicMock()
    ctx.__aenter__ = AsyncMock(return_value=mock_client)
    ctx.__aexit__ = AsyncMock(return_value=False)
    return ctx, mock_client


class TestFetchToken:
    async def test_returns_token_field(self):
        ctx, _ = _make_mock_http_client({"token": "bearer-tok", "expires_in": 300})
        with patch("httpx.AsyncClient", return_value=ctx):
            token = await fetch_token("https://auth.example.com", "svc", "scope")
        assert token == "bearer-tok"

    async def test_falls_back_to_access_token_field(self):
        ctx, _ = _make_mock_http_client({"access_token": "access-tok", "expires_in": 300})
        with patch("httpx.AsyncClient", return_value=ctx):
            token = await fetch_token("https://auth.example.com", "svc", "scope")
        assert token == "access-tok"

    async def test_returns_none_when_response_missing_token_field(self):
        ctx, _ = _make_mock_http_client({"not_token": "value", "expires_in": 300})
        with patch("httpx.AsyncClient", return_value=ctx):
            token = await fetch_token("https://auth.example.com", "svc", "scope")
        assert token is None

    async def test_defaults_expires_in_to_300_when_missing(self):
        ctx, _ = _make_mock_http_client({"token": "tok"})  # no expires_in key
        before = time.time()
        with patch("httpx.AsyncClient", return_value=ctx):
            token = await fetch_token("https://auth.example.com", "svc", "scope")
        assert token == "tok"
        key = _cache_key("https://auth.example.com", "svc", "scope", None)
        _, expires_at = docker_auth._token_cache[key]
        # Default expires_in=300, stored as time.time() + max(300-30, 10) = 270
        assert before + 268 <= expires_at <= before + 272

    async def test_uses_cache_on_second_call_without_http(self):
        ctx, mock_client = _make_mock_http_client({"token": "cached-tok", "expires_in": 300})
        with patch("httpx.AsyncClient", return_value=ctx):
            await fetch_token("https://auth.example.com", "svc", "scope")
            mock_client.get.reset_mock()
            token = await fetch_token("https://auth.example.com", "svc", "scope")
            mock_client.get.assert_not_called()
        assert token == "cached-tok"

    async def test_returns_none_on_network_error(self):
        mock_client = AsyncMock()
        mock_client.get = AsyncMock(side_effect=Exception("connection refused"))
        ctx = MagicMock()
        ctx.__aenter__ = AsyncMock(return_value=mock_client)
        ctx.__aexit__ = AsyncMock(return_value=False)
        with patch("httpx.AsyncClient", return_value=ctx):
            token = await fetch_token("https://auth.example.com", "svc", "scope")
        assert token is None

    async def test_returns_none_on_http_status_error(self):
        mock_response = MagicMock()
        mock_response.raise_for_status.side_effect = httpx.HTTPStatusError("401 Unauthorized", request=MagicMock(), response=MagicMock())
        mock_client = AsyncMock()
        mock_client.get = AsyncMock(return_value=mock_response)
        ctx = MagicMock()
        ctx.__aenter__ = AsyncMock(return_value=mock_client)
        ctx.__aexit__ = AsyncMock(return_value=False)
        with patch("httpx.AsyncClient", return_value=ctx):
            token = await fetch_token("https://auth.example.com", "svc", "scope")
        assert token is None

    async def test_passes_credentials_as_auth_tuple(self):
        ctx, mock_client = _make_mock_http_client({"token": "authed-tok", "expires_in": 300})
        with patch("httpx.AsyncClient", return_value=ctx):
            await fetch_token("https://auth.example.com", "svc", "scope", "user", "pass")
        call_kwargs = mock_client.get.call_args.kwargs
        assert call_kwargs.get("auth") == ("user", "pass")

    async def test_no_auth_when_no_credentials(self):
        ctx, mock_client = _make_mock_http_client({"token": "anon-tok", "expires_in": 300})
        with patch("httpx.AsyncClient", return_value=ctx):
            await fetch_token("https://auth.example.com", "svc", "scope")
        call_kwargs = mock_client.get.call_args.kwargs
        assert call_kwargs.get("auth") is None


# ---------------------------------------------------------------------------
# get_docker_token_for_response
# ---------------------------------------------------------------------------


class TestGetDockerTokenForResponse:
    async def test_returns_none_for_non_bearer_header(self):
        token = await get_docker_token_for_response('Basic realm="example"')
        assert token is None

    async def test_end_to_end_parse_and_fetch(self):
        """parse_www_authenticate → fetch_token wired together end-to-end."""
        header = 'Bearer realm="https://auth.example.com",service="svc",scope="repo:pull"'
        ctx, mock_client = _make_mock_http_client({"token": "e2e-tok", "expires_in": 300})
        with patch("httpx.AsyncClient", return_value=ctx):
            token = await get_docker_token_for_response(header, "user", "pass")
        assert token == "e2e-tok"
        call_kwargs = mock_client.get.call_args.kwargs
        assert call_kwargs["params"]["service"] == "svc"
        assert call_kwargs["params"]["scope"] == "repo:pull"
        assert call_kwargs["auth"] == ("user", "pass")
