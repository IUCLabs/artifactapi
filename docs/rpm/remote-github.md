# `github_rpm` remote

Shared classification, TTL and eviction rules: [caching](../common/caching.md).

| Type | Mutable (auto-detected) | Immutable (auto-detected) |
|---|---|---|
| `github_rpm` | `repodata/*` (synthesized) | `.rpm` (redirected) |

## GitHub releases as a yum repo (metadata-only, no precache)

A `github_rpm` remote turns a GitHub repo's **releases** into a real `dnf`/`yum`
repository without ever caching the packages. It scans releases for `.rpm`
assets, derives each package's metadata (NEVRA, requires/provides/conflicts/
obsoletes, files, checksum) and **synthesizes `repodata/` on the fly**. Package
metadata comes from a **ranged GET of just the RPM header** (the header sits at
the front of the file, so the whole package is never downloaded); the sha256
checksum comes from the GitHub asset `digest` when present, else a one-time
lazy stream. Derived metadata is cached (keyed by asset) so repodata generation
is served from primed DB rows, never a cold on-demand derive.

Each package's `<location>` points back at the remote, which **302-redirects**
the download to the `releases_remote` — an existing generic `github.com` remote
that streams the actual bytes. `dnf` follows the redirect transparently.

### Background syncer

A single process-wide **background syncer** keeps every `github_rpm` remote's
derived metadata current off the client request path:

- **Prime on create.** Creating a `github_rpm` remote enqueues a background prime
  scan, so its metadata is derived right away without blocking the create call.
  The first `dnf` request is served from cache. If a request arrives before the
  prime lands, it returns a retryable `503` (with `Retry-After`) rather than
  serving an empty repo or blocking on a multi-minute derive.
- **Periodic re-check, driven by `sync_interval` or `mutable_ttl`.** With
  `sync_interval` (seconds) set, a remote re-checks once per interval at a
  stable per-remote offset, so remotes on the same interval spread out instead
  of syncing together; its first scheduled sync waits for that slot (a
  cold-cache request still primes at once). Unset, it re-checks no more often
  than its `mutable_ttl`. New/changed
  assets are derived incrementally; assets already cached are never re-fetched,
  and assets that disappear upstream are pruned.
- **ETag / 304 conditional requests.** The releases-list `ETag` is stored per
  remote and sent as `If-None-Match`; a `304 Not Modified` means nothing changed
  and the syncer derives nothing. GitHub does not count `304` conditional
  responses against the rate limit, so an unchanged repo is nearly free — this is
  the main lever keeping GitHub traffic low.
- **Global rate limit.** Every GitHub call (releases list + each ranged asset
  header GET) passes through a single token-bucket limiter **shared across all
  remotes**, so GitHub is never hammered. Configure a token (`password`) on the
  remote for the higher authenticated rate limit (~5000/hr vs ~60/hr
  unauthenticated).
- **Multi-replica coordination.** State is shared through the database. Before a
  periodic scan a replica must atomically claim a per-remote lease
  (`github_rpm_sync_state`: `last_synced_at`, `etag`, `sync_lease_owner`,
  `sync_lease_expires`); only the winner scans. This bounds total GitHub load to
  ~once per `sync_interval` (else `mutable_ttl`) regardless of replica count, and the shared `etag`
  lets any replica issue the conditional request.

```hcl
# Backend that serves the actual .rpm bytes from github.com.
resource "artifactapi_remote_generic" "github" {
  name     = "github"
  base_url = "https://github.com"
  patterns = [
    "acme/tools/releases/download/.*\\.rpm$", # allowlist the repo's release assets
  ]
}

resource "artifactapi_remote_github_rpm" "acme-tools" {
  name            = "acme-tools"
  base_url        = "https://api.github.com/repos/acme/tools" # the releases API root
  releases_remote = "github"                                  # backend for downloads
  mutable_ttl     = 3600                                      # release re-scan interval
  cache_repodata  = true                                      # keep generated repodata in memory

  # Optional: restrict which release assets become packages (regex on filename).
  patterns = [".*\\.x86_64\\.rpm$", ".*\\.noarch\\.rpm$"]

  # Optional: a token for private repos / higher API rate limits.
  # password = "ghp_..."
}
```

`dnf` config: `baseurl=https://artifactapi.example/api/v1/remote/acme-tools`.
The repo is multi-arch (no `$basearch` needed) — `dnf` selects matching packages
from the synthesized metadata.

### Repodata cache

Repodata is regenerated from the metadata rows on every request by default. Set
`cache_repodata = true` on the remote to keep the generated `repomd.xml` and its
`primary`/`filelists`/`other` files in memory per replica. Each request checks a
cheap row fingerprint (count + newest id) in the database and rebuilds only when
it moved, so every replica picks up a scan's changes on its next request.
