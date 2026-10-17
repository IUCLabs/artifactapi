# `gems` remote

Shared classification, TTL and eviction rules: [caching](../common/caching.md).

| Type | Mutable (auto-detected) | Immutable (auto-detected) |
|---|---|---|
| `gems` | compact index (`versions`, `info/*`, `names`), `*specs.4.8.gz` | `gems/*.gem`, `quick/Marshal.4.8/*.gemspec.rz` |
