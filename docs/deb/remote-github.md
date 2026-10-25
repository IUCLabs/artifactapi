# `github_deb` remote

Shared classification, TTL and eviction rules: [caching](../common/caching.md).

| Type | Mutable (auto-detected) | Immutable (auto-detected) |
|---|---|---|
| `github_deb` | `Packages*`, `Release*` (synthesized) | `.deb` (redirected) |

## Release sync

A background syncer re-scans the GitHub releases list per remote, sharing one
lease per remote across replicas through `github_deb_sync_state`.

- **`sync_interval`** (seconds) re-checks once per interval at a stable
  per-remote offset, so remotes on the same interval spread out instead of
  syncing together; its first scheduled sync waits for that slot (a cold-cache
  request still primes at once). Unset, it re-checks no more often than its
  `mutable_ttl`.
- A failed scan keeps serving the last good index and retries with exponential
  backoff.
