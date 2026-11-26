# `pypi` remote

Shared classification, TTL and eviction rules: [caching](../common/caching.md).

| Type | Mutable (auto-detected) | Immutable (auto-detected) |
|---|---|---|
| `pypi` | `simple/*` index pages, `pypi/<pkg>[/<ver>]/json` | `.whl`, `.tar.gz` |

File links in served indexes point back at the remote, so downloads are cached.
