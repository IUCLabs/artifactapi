# `terraform` remote

Shared classification, TTL and eviction rules: [caching](../common/caching.md).

| Type | Mutable (auto-detected) | Immutable (auto-detected) |
|---|---|---|
| `terraform` | `*/versions`, `*/*/*/download/*/*` | none |

Set `releases_remote` to a `generic` remote for the archive host (e.g.
`https://releases.hashicorp.com`) to point the download document's
`download_url`, `shasums_url` and `shasums_signature_url` at it, so provider
zips are cached there.
