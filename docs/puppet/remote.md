# `puppet` remote

Shared classification, TTL and eviction rules: [caching](../common/caching.md).

| Type | Mutable (auto-detected) | Immutable (auto-detected) |
|---|---|---|
| `puppet` | `v3/modules/*`, `v3/releases*` | `.tar.gz` |

`/v3/releases` forwards the `module`, `limit` and `offset` query params
upstream; set `base_url` to the Forge API root, e.g. `https://forgeapi.puppet.com`.
