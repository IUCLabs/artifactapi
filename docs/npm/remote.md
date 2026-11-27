# `npm` remote

Shared classification, TTL and eviction rules: [caching](../common/caching.md).

| Type | Mutable (auto-detected) | Immutable (auto-detected) |
|---|---|---|
| `npm` | package metadata | `.tgz` tarballs |

Tarball URLs in served packuments point back at the remote, so tarballs are cached.
