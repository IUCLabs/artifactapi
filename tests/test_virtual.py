"""Unit tests for the virtual repository handler (artifact/virtual.py)."""

from datetime import UTC, date, datetime
from unittest.mock import AsyncMock, MagicMock, patch

import pytest
import yaml

from artifactapi.artifact.virtual import (
    _HANDLERS,
    _entries_to_msgpack_safe,
    _get_member_index,
    _HelmDumper,
    _HelmHandler,
    _merge_helm_indexes,
    _rewrite_urls,
    _VirtualHandler,
    _YamlDumperBase,
    _YamlLoader,
)

# ---------------------------------------------------------------------------
# Shared sample data
# ---------------------------------------------------------------------------

_INDEX_A = b"""\
apiVersion: v1
entries:
  vault:
    - name: vault
      version: "0.27.0"
      urls:
        - https://helm.releases.hashicorp.com/vault-0.27.0.tgz
  consul:
    - name: consul
      version: "1.2.0"
      urls:
        - https://helm.releases.hashicorp.com/consul-1.2.0.tgz
generated: "2023-01-01T00:00:00.000Z"
"""

_INDEX_B = b"""\
apiVersion: v1
entries:
  nginx:
    - name: nginx
      version: "15.0.0"
      urls:
        - https://charts.example.com/nginx-15.0.0.tgz
  vault:
    - name: vault
      version: "0.27.0"
      urls:
        - https://charts.example.com/vault-0.27.0.tgz
    - name: vault
      version: "0.26.0"
      urls:
        - https://charts.example.com/vault-0.26.0.tgz
generated: "2023-01-01T00:00:00.000Z"
"""

_INDEX_SIMPLE = b"""\
apiVersion: v1
entries:
  mychart:
    - name: mychart
      version: "1.0.0"
      urls:
        - https://helm.releases.hashicorp.com/mychart-1.0.0.tgz
generated: "2023-01-01T00:00:00.000Z"
"""

_INDEX_RELATIVE = b"""\
apiVersion: v1
entries:
  rancher:
    - name: rancher
      version: "2.13.1"
      urls:
        - rancher-2.13.1.tgz
generated: "2023-01-01T00:00:00.000Z"
"""

_CFG_A = {"base_url": "https://helm.releases.hashicorp.com", "cache": {"mutable_ttl": 3600}}
_CFG_B = {"base_url": "https://charts.example.com", "cache": {"mutable_ttl": 1800}}


# ---------------------------------------------------------------------------
# _YamlLoader / _YamlDumperBase — C extension selection
# ---------------------------------------------------------------------------


class TestYamlExtensionSelection:
    def test_loader_is_a_class(self):
        assert isinstance(_YamlLoader, type)

    def test_dumper_base_is_a_class(self):
        assert isinstance(_YamlDumperBase, type)

    def test_helm_dumper_uses_selected_base(self):
        assert issubclass(_HelmDumper, _YamlDumperBase)

    def test_c_extensions_used_when_available(self):
        try:
            assert _YamlLoader is yaml.CSafeLoader
            assert _YamlDumperBase is yaml.CDumper
        except AttributeError:
            assert _YamlLoader is yaml.SafeLoader
            assert _YamlDumperBase is yaml.Dumper

    def test_loader_can_parse_yaml(self):
        result = yaml.load(b"key: value", Loader=_YamlLoader)
        assert result == {"key": "value"}


# ---------------------------------------------------------------------------
# _HelmDumper — datetime/date YAML serialization
# ---------------------------------------------------------------------------


class TestHelmDumper:
    def _dump(self, value):
        return yaml.dump({"v": value}, Dumper=_HelmDumper)

    def test_datetime_with_tz_includes_Z_suffix(self):
        dt = datetime(2023, 6, 15, 12, 0, 0, tzinfo=UTC)
        assert "Z" in self._dump(dt)

    def test_datetime_without_tz_has_no_Z_suffix(self):
        dt = datetime(2023, 6, 15, 12, 0, 0)
        assert "Z" not in self._dump(dt)

    def test_datetime_uses_T_separator_not_space(self):
        dt = datetime(2023, 6, 15, 12, 30, 0, tzinfo=UTC)
        assert "T12:30:00" in self._dump(dt)

    def test_date_serialized_as_iso_string(self):
        assert "2023-01-15" in self._dump(date(2023, 1, 15))

    def test_datetime_round_trips_as_string_not_python_datetime(self):
        dt = datetime(2023, 6, 15, 12, 0, 0, tzinfo=UTC)
        parsed = yaml.safe_load(self._dump(dt))
        # yaml.safe_load must not re-parse this as a datetime object
        assert isinstance(parsed["v"], str)

    def test_date_round_trips_as_string_not_python_date(self):
        parsed = yaml.safe_load(self._dump(date(2023, 1, 15)))
        assert isinstance(parsed["v"], str)


# ---------------------------------------------------------------------------
# _HelmHandler
# ---------------------------------------------------------------------------


class TestHelmHandler:
    def setup_method(self):
        self.handler = _HelmHandler()

    def test_accepts_index_yaml(self):
        assert self.handler.accepts_path("index.yaml") is True

    def test_rejects_tgz_path(self):
        assert self.handler.accepts_path("vault-0.27.0.tgz") is False

    def test_rejects_subdirectory_index(self):
        assert self.handler.accepts_path("charts/index.yaml") is False

    def test_rejects_empty_path(self):
        assert self.handler.accepts_path("") is False

    def test_path_error_is_non_empty_string(self):
        msg = self.handler.path_error()
        assert isinstance(msg, str) and len(msg) > 0

    def test_merge_returns_bytes(self):
        result = self.handler.merge([_INDEX_A], [None], ["member-a"], [_CFG_A], "http://proxy.example.com")
        assert isinstance(result, bytes)

    def test_merge_delegates_to_merge_helm_indexes(self):
        with patch("artifactapi.artifact.virtual._merge_helm_indexes", return_value=b"merged") as mock_fn:
            result = self.handler.merge([b"data"], [None], ["m"], [{}], "http://proxy")
        mock_fn.assert_called_once_with([b"data"], [None], ["m"], [{}], "http://proxy")
        assert result == b"merged"


# ---------------------------------------------------------------------------
# _HANDLERS registry
# ---------------------------------------------------------------------------


class TestHandlersRegistry:
    def test_helm_handler_is_registered(self):
        assert "helm" in _HANDLERS
        assert isinstance(_HANDLERS["helm"], _HelmHandler)

    def test_helm_handler_satisfies_protocol(self):
        assert isinstance(_HANDLERS["helm"], _VirtualHandler)


# ---------------------------------------------------------------------------
# _rewrite_urls
# ---------------------------------------------------------------------------


class TestRewriteUrls:
    def _rewrite(self, urls, base_url="https://upstream.example.com", proxy_base="http://proxy.example.com", member_name="my-remote"):
        return _rewrite_urls(urls, base_url, proxy_base, member_name)

    def test_absolute_url_matching_base_is_rewritten(self):
        result = self._rewrite(["https://upstream.example.com/chart-1.0.0.tgz"])
        assert result == ["http://proxy.example.com/api/v1/remote/my-remote/chart-1.0.0.tgz"]

    def test_relative_url_is_prepended_with_proxy_remote(self):
        result = self._rewrite(["chart-1.0.0.tgz"])
        assert result == ["http://proxy.example.com/api/v1/remote/my-remote/chart-1.0.0.tgz"]

    def test_relative_url_with_leading_slash(self):
        result = self._rewrite(["/chart-1.0.0.tgz"])
        assert result == ["http://proxy.example.com/api/v1/remote/my-remote/chart-1.0.0.tgz"]

    def test_absolute_url_not_matching_base_is_unchanged(self):
        result = self._rewrite(["https://other.example.com/chart-1.0.0.tgz"])
        assert result == ["https://other.example.com/chart-1.0.0.tgz"]

    def test_empty_url_list_returns_empty(self):
        assert self._rewrite([]) == []

    def test_multiple_urls_all_rewritten(self):
        urls = ["https://upstream.example.com/a-1.0.0.tgz", "b-2.0.0.tgz"]
        result = self._rewrite(urls)
        assert result[0] == "http://proxy.example.com/api/v1/remote/my-remote/a-1.0.0.tgz"
        assert result[1] == "http://proxy.example.com/api/v1/remote/my-remote/b-2.0.0.tgz"


# ---------------------------------------------------------------------------
# _merge_helm_indexes
# ---------------------------------------------------------------------------


class TestMergeHelmIndexes:
    def _merge(self, raw_indexes, member_names, member_configs, proxy_base="http://proxy.example.com"):
        return _merge_helm_indexes(raw_indexes, [None] * len(raw_indexes), member_names, member_configs, proxy_base)

    def _parse(self, raw):
        return yaml.safe_load(raw)

    def test_single_member_all_charts_present(self):
        index = self._parse(self._merge([_INDEX_A], ["member-a"], [_CFG_A]))
        assert "vault" in index["entries"]
        assert "consul" in index["entries"]

    def test_two_members_non_overlapping_charts_all_present(self):
        index = self._parse(self._merge([_INDEX_A, _INDEX_B], ["member-a", "member-b"], [_CFG_A, _CFG_B]))
        assert "vault" in index["entries"]
        assert "consul" in index["entries"]
        assert "nginx" in index["entries"]

    def test_first_member_wins_on_duplicate_name_and_version(self):
        index = self._parse(self._merge([_INDEX_A, _INDEX_B], ["member-a", "member-b"], [_CFG_A, _CFG_B]))
        v027 = next(e for e in index["entries"]["vault"] if e["version"] == "0.27.0")
        assert "member-a" in v027["urls"][0]

    def test_absolute_urls_rewritten_to_proxy(self):
        index = self._parse(self._merge([_INDEX_A], ["member-a"], [_CFG_A]))
        url = index["entries"]["vault"][0]["urls"][0]
        assert url == "http://proxy.example.com/api/v1/remote/member-a/vault-0.27.0.tgz"

    def test_relative_urls_rewritten_to_proxy(self):
        cfg = {"base_url": "https://releases.rancher.com/server-charts/stable", "cache": {"mutable_ttl": 3600}}
        index = self._parse(self._merge([_INDEX_RELATIVE], ["rancher-stable"], [cfg]))
        url = index["entries"]["rancher"][0]["urls"][0]
        assert url == "http://proxy.example.com/api/v1/remote/rancher-stable/rancher-2.13.1.tgz"

    def test_different_versions_of_same_chart_both_included(self):
        index = self._parse(self._merge([_INDEX_A, _INDEX_B], ["member-a", "member-b"], [_CFG_A, _CFG_B]))
        versions = {e["version"] for e in index["entries"]["vault"]}
        assert "0.27.0" in versions
        assert "0.26.0" in versions

    def test_malformed_yaml_from_member_is_skipped(self):
        index = self._parse(self._merge([_INDEX_A, b"{bad yaml"], ["member-a", "bad"], [_CFG_A, _CFG_B]))
        assert "vault" in index["entries"]
        assert "consul" in index["entries"]

    def test_output_has_apiVersion_v1(self):
        index = self._parse(self._merge([_INDEX_A], ["member-a"], [_CFG_A]))
        assert index["apiVersion"] == "v1"

    def test_output_has_generated_field(self):
        index = self._parse(self._merge([_INDEX_A], ["member-a"], [_CFG_A]))
        assert "generated" in index

    def test_output_is_valid_yaml(self):
        raw = self._merge([_INDEX_A, _INDEX_B], ["member-a", "member-b"], [_CFG_A, _CFG_B])
        assert isinstance(yaml.safe_load(raw), dict)

    def test_empty_index_from_member_produces_no_entries(self):
        empty = b"apiVersion: v1\nentries: {}\ngenerated: '2023-01-01T00:00:00.000Z'\n"
        index = self._parse(self._merge([empty], ["member-a"], [_CFG_A]))
        assert index["entries"] == {}


# ---------------------------------------------------------------------------
# _get_member_index  (async)
# ---------------------------------------------------------------------------


class TestGetMemberIndex:
    @pytest.fixture
    def storage(self):
        m = MagicMock()
        m.get_object_key.return_value = "member/key/index.yaml"
        m.exists.return_value = False
        m.download_object.return_value = b"cached bytes"
        return m

    @pytest.fixture
    def cache(self):
        m = MagicMock()
        m.is_index_valid.return_value = False
        return m

    @pytest.fixture
    def member_cfg(self):
        return {"base_url": "https://helm.releases.hashicorp.com", "cache": {"mutable_ttl": 3600}}

    def _fake_response(self, content=b"upstream bytes"):
        r = MagicMock()
        r.content = content
        r.raise_for_status = MagicMock()
        return r

    def _patch_httpx(self, response):
        mock_client_cls = patch("artifactapi.artifact.virtual.httpx.AsyncClient")
        p = mock_client_cls.start()
        mock_client = AsyncMock()
        p.return_value.__aenter__.return_value = mock_client
        mock_client.get.return_value = response
        return mock_client_cls, mock_client

    async def test_cache_hit_returns_stored_bytes(self, storage, cache, member_cfg):
        storage.exists.return_value = True
        cache.is_index_valid.return_value = True

        _, _, _, raw_data, _ = await _get_member_index("m", member_cfg, "index.yaml", storage, cache)

        assert raw_data == b"cached bytes"

    async def test_cache_hit_does_not_fetch_upstream(self, storage, cache, member_cfg):
        storage.exists.return_value = True
        cache.is_index_valid.return_value = True

        with patch("artifactapi.artifact.virtual.httpx.AsyncClient") as mock_cls:
            await _get_member_index("m", member_cfg, "index.yaml", storage, cache)

        mock_cls.assert_not_called()

    async def test_cache_hit_storage_error_falls_through_to_upstream(self, storage, cache, member_cfg):
        storage.exists.return_value = True
        cache.is_index_valid.return_value = True
        storage.download_object.side_effect = Exception("S3 read error")

        with patch("artifactapi.artifact.virtual.httpx.AsyncClient") as mock_cls:
            mock_client = AsyncMock()
            mock_cls.return_value.__aenter__.return_value = mock_client
            mock_client.get.return_value = self._fake_response(b"fresh bytes")

            _, _, _, raw_data, _ = await _get_member_index("m", member_cfg, "index.yaml", storage, cache)

        assert raw_data == b"fresh bytes"

    async def test_cache_miss_fetches_from_upstream(self, storage, cache, member_cfg):
        with patch("artifactapi.artifact.virtual.httpx.AsyncClient") as mock_cls:
            mock_client = AsyncMock()
            mock_cls.return_value.__aenter__.return_value = mock_client
            mock_client.get.return_value = self._fake_response()

            _, _, _, raw_data, _ = await _get_member_index("m", member_cfg, "index.yaml", storage, cache)

        assert raw_data == b"upstream bytes"

    async def test_cache_miss_stores_result_in_s3(self, storage, cache, member_cfg):
        with patch("artifactapi.artifact.virtual.httpx.AsyncClient") as mock_cls:
            mock_client = AsyncMock()
            mock_cls.return_value.__aenter__.return_value = mock_client
            mock_client.get.return_value = self._fake_response()

            await _get_member_index("m", member_cfg, "index.yaml", storage, cache)

        storage.upload.assert_called_once()

    async def test_cache_miss_marks_cache_with_configured_ttl(self, storage, cache, member_cfg):
        with patch("artifactapi.artifact.virtual.httpx.AsyncClient") as mock_cls:
            mock_client = AsyncMock()
            mock_cls.return_value.__aenter__.return_value = mock_client
            mock_client.get.return_value = self._fake_response()

            await _get_member_index("m", member_cfg, "index.yaml", storage, cache)

        cache.mark_index_cached.assert_called_once_with("m", "index.yaml", 3600)

    async def test_cache_miss_with_auth_sends_basic_auth_header(self, storage, cache):
        cfg = {
            "base_url": "https://private.example.com",
            "username": "user",
            "password": "pass",
            "cache": {"mutable_ttl": 3600},
        }
        with patch("artifactapi.artifact.virtual.httpx.AsyncClient") as mock_cls:
            mock_client = AsyncMock()
            mock_cls.return_value.__aenter__.return_value = mock_client
            mock_client.get.return_value = self._fake_response()

            await _get_member_index("m", cfg, "index.yaml", storage, cache)

        headers = mock_client.get.call_args.kwargs["headers"]
        assert "Authorization" in headers
        assert headers["Authorization"].startswith("Basic ")

    async def test_no_credentials_sends_no_auth_header(self, storage, cache, member_cfg):
        with patch("artifactapi.artifact.virtual.httpx.AsyncClient") as mock_cls:
            mock_client = AsyncMock()
            mock_cls.return_value.__aenter__.return_value = mock_client
            mock_client.get.return_value = self._fake_response()

            await _get_member_index("m", member_cfg, "index.yaml", storage, cache)

        headers = mock_client.get.call_args.kwargs["headers"]
        assert "Authorization" not in headers

    async def test_upstream_fetch_failure_returns_none(self, storage, cache, member_cfg):
        with patch("artifactapi.artifact.virtual.httpx.AsyncClient") as mock_cls:
            mock_client = AsyncMock()
            mock_cls.return_value.__aenter__.return_value = mock_client
            mock_client.get.side_effect = Exception("connection refused")

            _, _, _, raw_data, _ = await _get_member_index("m", member_cfg, "index.yaml", storage, cache)

        assert raw_data is None

    async def test_s3_upload_failure_still_returns_data(self, storage, cache, member_cfg):
        storage.upload.side_effect = Exception("S3 write error")

        with patch("artifactapi.artifact.virtual.httpx.AsyncClient") as mock_cls:
            mock_client = AsyncMock()
            mock_cls.return_value.__aenter__.return_value = mock_client
            mock_client.get.return_value = self._fake_response()

            _, _, _, raw_data, _ = await _get_member_index("m", member_cfg, "index.yaml", storage, cache)

        assert raw_data == b"upstream bytes"

    async def test_returns_ttl_from_config(self, storage, cache):
        cfg = {"base_url": "https://example.com", "cache": {"mutable_ttl": 900}}
        with patch("artifactapi.artifact.virtual.httpx.AsyncClient") as mock_cls:
            mock_client = AsyncMock()
            mock_cls.return_value.__aenter__.return_value = mock_client
            mock_client.get.return_value = self._fake_response()

            _, _, ttl, _, _ = await _get_member_index("m", cfg, "index.yaml", storage, cache)

        assert ttl == 900

    async def test_defaults_ttl_to_3600_when_not_configured(self, storage, cache):
        cfg = {"base_url": "https://example.com"}
        with patch("artifactapi.artifact.virtual.httpx.AsyncClient") as mock_cls:
            mock_client = AsyncMock()
            mock_cls.return_value.__aenter__.return_value = mock_client
            mock_client.get.return_value = self._fake_response()

            _, _, ttl, _, _ = await _get_member_index("m", cfg, "index.yaml", storage, cache)

        assert ttl == 3600


# ---------------------------------------------------------------------------
# Virtual route  GET /api/v1/virtual/{name}/{path}
# ---------------------------------------------------------------------------


@pytest.fixture
def mock_storage_v():
    m = MagicMock()
    m.get_object_key.return_value = "virtual/helm-virtual-test/index.yaml"
    m.exists.return_value = False
    m.download_object.return_value = b"apiVersion: v1\nentries: {}\n"
    return m


@pytest.fixture
def mock_cache_v():
    m = MagicMock()
    m.is_index_valid.return_value = False
    m.available = False
    m.client = None
    return m


@pytest.fixture
def patched_virtual_deps(mock_storage_v, mock_cache_v):
    import artifactapi.main as main_mod

    with (
        patch.object(main_mod, "storage", mock_storage_v),
        patch.object(main_mod, "cache", mock_cache_v),
    ):
        yield {"storage": mock_storage_v, "cache": mock_cache_v}


class TestVirtualRoute:
    def test_unknown_virtual_name_returns_404(self, client, patched_virtual_deps):
        response = client.get("/api/v1/virtual/no-such-virtual/index.yaml")
        assert response.status_code == 404

    def test_non_virtual_name_returns_404(self, client, patched_virtual_deps):
        # helm-test is in remotes, not virtuals
        response = client.get("/api/v1/virtual/helm-test/index.yaml")
        assert response.status_code == 404

    def test_unsupported_package_returns_400(self, client, patched_virtual_deps):
        # unsupported-virtual-test has package "rpm"
        response = client.get("/api/v1/virtual/unsupported-virtual-test/index.yaml")
        assert response.status_code == 400

    def test_non_index_path_returns_404(self, client, patched_virtual_deps):
        response = client.get("/api/v1/virtual/helm-virtual-test/vault-0.27.0.tgz")
        assert response.status_code == 404

    def test_no_members_returns_500(self, client, patched_virtual_deps):
        response = client.get("/api/v1/virtual/empty-virtual-test/index.yaml")
        assert response.status_code == 500

    def test_virtual_cache_hit_returns_200(self, client, patched_virtual_deps):
        deps = patched_virtual_deps
        deps["storage"].exists.return_value = True
        deps["cache"].is_index_valid.return_value = True

        response = client.get("/api/v1/virtual/helm-virtual-test/index.yaml")
        assert response.status_code == 200

    def test_virtual_cache_hit_content_type_is_yaml(self, client, patched_virtual_deps):
        deps = patched_virtual_deps
        deps["storage"].exists.return_value = True
        deps["cache"].is_index_valid.return_value = True

        response = client.get("/api/v1/virtual/helm-virtual-test/index.yaml")
        assert "text/yaml" in response.headers["content-type"]

    def test_virtual_cache_hit_returns_stored_content(self, client, patched_virtual_deps):
        deps = patched_virtual_deps
        deps["storage"].exists.return_value = True
        deps["cache"].is_index_valid.return_value = True
        deps["storage"].download_object.return_value = b"apiVersion: v1\nentries: {}\n"

        response = client.get("/api/v1/virtual/helm-virtual-test/index.yaml")
        assert response.content == b"apiVersion: v1\nentries: {}\n"

    def test_virtual_cache_hit_skips_member_fetch(self, client, patched_virtual_deps):
        deps = patched_virtual_deps
        deps["storage"].exists.return_value = True
        deps["cache"].is_index_valid.return_value = True

        with patch("artifactapi.artifact.virtual._get_member_index", new_callable=AsyncMock) as mock_get:
            client.get("/api/v1/virtual/helm-virtual-test/index.yaml")

        mock_get.assert_not_called()

    def test_cache_miss_returns_200_with_yaml_content_type(self, client, patched_virtual_deps):
        with patch("artifactapi.artifact.virtual._get_member_index", new_callable=AsyncMock) as mock_get:
            mock_get.return_value = ("helm-test", _CFG_A, 3600, _INDEX_SIMPLE, None)
            response = client.get("/api/v1/virtual/helm-virtual-test/index.yaml")

        assert response.status_code == 200
        assert "text/yaml" in response.headers["content-type"]

    def test_cache_miss_response_contains_merged_entries(self, client, patched_virtual_deps):
        with patch("artifactapi.artifact.virtual._get_member_index", new_callable=AsyncMock) as mock_get:
            mock_get.return_value = ("helm-test", _CFG_A, 3600, _INDEX_SIMPLE, None)
            response = client.get("/api/v1/virtual/helm-virtual-test/index.yaml")

        index = yaml.safe_load(response.content)
        assert "mychart" in index["entries"]

    def test_cache_miss_stores_result_in_s3(self, client, patched_virtual_deps):
        deps = patched_virtual_deps
        with patch("artifactapi.artifact.virtual._get_member_index", new_callable=AsyncMock) as mock_get:
            mock_get.return_value = ("helm-test", _CFG_A, 3600, _INDEX_SIMPLE, None)
            client.get("/api/v1/virtual/helm-virtual-test/index.yaml")

        deps["storage"].upload.assert_called_once()

    def test_cache_miss_marks_index_cached(self, client, patched_virtual_deps):
        deps = patched_virtual_deps
        with patch("artifactapi.artifact.virtual._get_member_index", new_callable=AsyncMock) as mock_get:
            mock_get.return_value = ("helm-test", _CFG_A, 3600, _INDEX_SIMPLE, None)
            client.get("/api/v1/virtual/helm-virtual-test/index.yaml")

        deps["cache"].mark_index_cached.assert_called_once()

    def test_cache_miss_uses_min_ttl_across_members(self, client, patched_virtual_deps):
        deps = patched_virtual_deps
        with patch("artifactapi.artifact.virtual._get_member_index", new_callable=AsyncMock) as mock_get:
            mock_get.side_effect = [
                ("helm-test", _CFG_A, 3600, _INDEX_SIMPLE, None),
                ("helm-member-2", _CFG_B, 1800, _INDEX_SIMPLE, None),
            ]
            client.get("/api/v1/virtual/helm-virtual-test/index.yaml")

        _, _, ttl = deps["cache"].mark_index_cached.call_args[0]
        assert ttl == 1800

    def test_all_members_unreachable_returns_502(self, client, patched_virtual_deps):
        with patch("artifactapi.artifact.virtual._get_member_index", new_callable=AsyncMock) as mock_get:
            mock_get.return_value = ("helm-test", _CFG_A, 3600, None, None)
            response = client.get("/api/v1/virtual/helm-virtual-test/index.yaml")

        assert response.status_code == 502

    def test_one_member_unreachable_still_returns_200(self, client, patched_virtual_deps):
        with patch("artifactapi.artifact.virtual._get_member_index", new_callable=AsyncMock) as mock_get:
            mock_get.side_effect = [
                ("helm-test", _CFG_A, 3600, _INDEX_SIMPLE, None),
                ("helm-member-2", _CFG_B, 1800, None, None),
            ]
            response = client.get("/api/v1/virtual/helm-virtual-test/index.yaml")

        assert response.status_code == 200

    def test_member_not_in_config_is_skipped(self, client, patched_virtual_deps):
        import artifactapi.main as main_mod

        real_get = main_mod.config.get_remote_config

        def patched_get(name):
            return None if name == "helm-member-2" else real_get(name)

        with (
            patch("artifactapi.artifact.virtual._get_member_index", new_callable=AsyncMock) as mock_get,
            patch.object(main_mod.config, "get_remote_config", side_effect=patched_get),
        ):
            mock_get.return_value = ("helm-test", _CFG_A, 3600, _INDEX_SIMPLE, None)
            response = client.get("/api/v1/virtual/helm-virtual-test/index.yaml")

        # only helm-test was available — should succeed
        assert response.status_code == 200
        mock_get.assert_called_once()

    def test_s3_store_failure_still_returns_200(self, client, patched_virtual_deps):
        deps = patched_virtual_deps
        deps["storage"].upload.side_effect = Exception("S3 write error")

        with patch("artifactapi.artifact.virtual._get_member_index", new_callable=AsyncMock) as mock_get:
            mock_get.return_value = ("helm-test", _CFG_A, 3600, _INDEX_SIMPLE, None)
            response = client.get("/api/v1/virtual/helm-virtual-test/index.yaml")

        assert response.status_code == 200


# ---------------------------------------------------------------------------
# _entries_to_msgpack_safe
# ---------------------------------------------------------------------------


class TestEntriesToMsgpackSafe:
    def test_plain_string_values_pass_through(self):
        entries = {"chart": [{"name": "chart", "version": "1.0.0", "urls": ["http://x/c.tgz"]}]}
        result = _entries_to_msgpack_safe(entries)
        assert result["chart"][0]["version"] == "1.0.0"

    def test_datetime_converted_to_iso_string(self):
        dt = datetime(2023, 6, 15, 12, 0, 0, tzinfo=UTC)
        entries = {"chart": [{"name": "chart", "version": "1.0.0", "created": dt}]}
        result = _entries_to_msgpack_safe(entries)
        assert isinstance(result["chart"][0]["created"], str)
        assert "2023-06-15" in result["chart"][0]["created"]

    def test_date_converted_to_iso_string(self):
        entries = {"chart": [{"name": "chart", "version": "1.0.0", "created": date(2023, 6, 15)}]}
        result = _entries_to_msgpack_safe(entries)
        assert result["chart"][0]["created"] == "2023-06-15"

    def test_empty_entries_returns_empty_dict(self):
        assert _entries_to_msgpack_safe({}) == {}

    def test_multiple_versions_all_converted(self):
        dt = datetime(2023, 1, 1, tzinfo=UTC)
        entries = {
            "chart": [
                {"name": "chart", "version": "1.0.0", "created": dt},
                {"name": "chart", "version": "2.0.0", "created": dt},
            ]
        }
        result = _entries_to_msgpack_safe(entries)
        for v in result["chart"]:
            assert isinstance(v["created"], str)

    def test_result_is_msgpack_serializable(self):
        import msgpack

        dt = datetime(2023, 6, 15, 12, 0, 0, tzinfo=UTC)
        entries = {"chart": [{"name": "chart", "version": "1.0.0", "created": dt, "urls": ["http://x/c.tgz"]}]}
        safe = _entries_to_msgpack_safe(entries)
        packed = msgpack.packb(safe, use_bin_type=True)
        unpacked = msgpack.unpackb(packed, raw=False)
        assert unpacked["chart"][0]["created"] == safe["chart"][0]["created"]


# ---------------------------------------------------------------------------
# _merge_helm_indexes — pre-parsed entries path
# ---------------------------------------------------------------------------


class TestMergeHelmIndexesWithParsed:
    """Verify that pre-parsed entries (from msgpack) produce the same output as raw YAML."""

    def _parse_entries(self, raw: bytes) -> dict:
        index = yaml.safe_load(raw)
        return index.get("entries") or {}

    def test_parsed_entries_produce_same_charts_as_raw(self):
        parsed = self._parse_entries(_INDEX_A)
        raw_result = yaml.safe_load(_merge_helm_indexes([_INDEX_A], [None], ["member-a"], [_CFG_A], "http://proxy.example.com"))
        parsed_result = yaml.safe_load(_merge_helm_indexes([_INDEX_A], [parsed], ["member-a"], [_CFG_A], "http://proxy.example.com"))
        assert set(raw_result["entries"].keys()) == set(parsed_result["entries"].keys())

    def test_parsed_entries_urls_are_rewritten(self):
        parsed = self._parse_entries(_INDEX_A)
        result = yaml.safe_load(_merge_helm_indexes([_INDEX_A], [parsed], ["member-a"], [_CFG_A], "http://proxy.example.com"))
        url = result["entries"]["vault"][0]["urls"][0]
        assert "member-a" in url
        assert "proxy.example.com" in url

    def test_none_parsed_falls_back_to_raw_bytes(self):
        result = yaml.safe_load(_merge_helm_indexes([_INDEX_A], [None], ["member-a"], [_CFG_A], "http://proxy.example.com"))
        assert "vault" in result["entries"]

    def test_mixed_parsed_and_raw_merge_correctly(self):
        parsed_a = self._parse_entries(_INDEX_A)
        result = yaml.safe_load(
            _merge_helm_indexes(
                [_INDEX_A, _INDEX_B],
                [parsed_a, None],
                ["member-a", "member-b"],
                [_CFG_A, _CFG_B],
                "http://proxy.example.com",
            )
        )
        assert "vault" in result["entries"]
        assert "nginx" in result["entries"]


# ---------------------------------------------------------------------------
# _get_member_index — msgpack cache behaviour
# ---------------------------------------------------------------------------


class TestGetMemberIndexMsgpack:
    @pytest.fixture
    def storage(self):
        m = MagicMock()
        m.get_object_key.side_effect = lambda name, path: f"{name}/{path}"
        m.exists.return_value = False
        m.download_object.return_value = _INDEX_SIMPLE
        return m

    @pytest.fixture
    def cache(self):
        m = MagicMock()
        m.is_index_valid.return_value = False
        return m

    @pytest.fixture
    def member_cfg(self):
        return {"base_url": "https://helm.releases.hashicorp.com", "cache": {"mutable_ttl": 3600}}

    def _fake_response(self, content=_INDEX_SIMPLE):
        r = MagicMock()
        r.content = content
        r.raise_for_status = MagicMock()
        return r

    async def test_cache_hit_with_msgpack_returns_parsed_entries(self, storage, cache, member_cfg):
        import msgpack

        entries = {"mychart": [{"name": "mychart", "version": "1.0.0", "urls": ["http://x/c.tgz"]}]}
        packed = msgpack.packb(entries, use_bin_type=True)

        storage.exists.side_effect = lambda key: True
        cache.is_index_valid.return_value = True
        storage.download_object.side_effect = lambda key: packed if key.endswith("index.msgpack") else _INDEX_SIMPLE

        _, _, _, raw_data, parsed = await _get_member_index("m", member_cfg, "index.yaml", storage, cache)

        assert parsed == entries

    async def test_cache_miss_builds_msgpack_and_returns_parsed(self, storage, cache, member_cfg):
        with patch("artifactapi.artifact.virtual.httpx.AsyncClient") as mock_cls:
            mock_client = AsyncMock()
            mock_cls.return_value.__aenter__.return_value = mock_client
            mock_client.get.return_value = self._fake_response()

            _, _, _, raw_data, parsed = await _get_member_index("m", member_cfg, "index.yaml", storage, cache)

        assert raw_data == _INDEX_SIMPLE
        assert isinstance(parsed, dict)
        assert "mychart" in parsed

    async def test_broken_msgpack_rebuilds_from_raw_yaml(self, storage, cache, member_cfg):
        storage.exists.side_effect = lambda key: True
        cache.is_index_valid.return_value = True
        storage.download_object.side_effect = lambda key: b"not-valid-msgpack" if key.endswith("index.msgpack") else _INDEX_SIMPLE

        _, _, _, raw_data, parsed = await _get_member_index("m", member_cfg, "index.yaml", storage, cache)

        assert raw_data == _INDEX_SIMPLE
        # Falls back to YAML parse and rebuilds msgpack — entries are returned
        assert isinstance(parsed, dict)
        assert "mychart" in parsed

    async def test_upstream_failure_returns_none_for_both(self, storage, cache, member_cfg):
        with patch("artifactapi.artifact.virtual.httpx.AsyncClient") as mock_cls:
            mock_client = AsyncMock()
            mock_cls.return_value.__aenter__.return_value = mock_client
            mock_client.get.side_effect = Exception("timeout")

            _, _, _, raw_data, parsed = await _get_member_index("m", member_cfg, "index.yaml", storage, cache)

        assert raw_data is None
        assert parsed is None
