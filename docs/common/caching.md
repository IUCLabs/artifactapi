# Caching and classification (all remotes)

Every proxied request on a remote is classified as **denied**, **immutable** or
**mutable**. The class picks the storage path and the freshness TTL.

## Fields

| Field | Meaning |
|---|---|
| `patterns` | Allowlist of regexes. Empty allows every path; otherwise a non-matching path is denied (403). |
| `blocklist` | Regexes; a match is denied (403). |
| `immutable_patterns` | Regexes forcing a path immutable. |
| `mutable_patterns` | Regexes forcing a path mutable. |
| `immutable_ttl` | Seconds an immutable path stays fresh. `0` = forever. |
| `mutable_ttl` | Seconds a mutable path stays fresh. `0` also means never expires. Omitted on create, it defaults to `3600`; omitted on update, the stored value is kept. |
| `check_mutable` | When a mutable path's `mutable_ttl` expires, sends a conditional `HEAD` (`If-None-Match`) with the stored upstream ETag. A `304` re-arms the TTL and serves the stored copy; anything else refetches in full. The ETag is kept for `max(10 × mutable_ttl, 7 days)`, re-armed on each `304`, so idle paths age out. It is only stored when `check_mutable` is set and `mutable_ttl` is non-zero, and never for immutable paths. |
| `stale_on_error` | On an upstream network error, serve the stored copy instead of failing. |
| `quarantine_enabled` / `quarantine_days` | Stored on the remote but not enforced by the proxy yet. |
| `mirrorlist` / `mirror_strategy` | rpm/deb/alpine only. Upstream pool is `[base_url] + mirrorlist`; `round_robin` (default) rotates the starting mirror, `least_conn` starts at the one with the fewest in-flight requests. Network errors and 5xx fail over to the next mirror; other statuses return as-is. |

## Precedence

The first rule that matches wins:

1. `blocklist` matches: **denied**
2. `patterns` is non-empty and nothing matches: **denied**
3. `immutable_patterns` matches: **immutable**
4. `mutable_patterns` matches: **mutable**
5. the provider's built-in `Classify` says mutable: **mutable**
6. anything else: **immutable**

So `blocklist` beats the allowlist, and `immutable_patterns` beats
`mutable_patterns`. Either user list overrides the built-in rules. `GET` and
`HEAD` both apply this order.

## Matching rules

- Regexes are Go `regexp`, matched **unanchored** (`MatchString`). Add `^` or `$` yourself when you need them.
- They match the path after `/api/v1/remote/<name>/`, without a leading slash, e.g. `9/BaseOS/x86_64/os/repodata/repomd.xml`.
- The query string is not part of the path. It is dropped and not forwarded upstream.
- `create` and `update` reject an invalid regex in `patterns`, `blocklist`, `mutable_patterns`, `immutable_patterns` or `ban_tags` with a 400. Only invalid regexes already stored before that check are skipped at match time.

## Lifecycles

| | Immutable | Mutable |
|---|---|---|
| Stored as | content-addressed blob (sha256), streamed | per-path index object, buffered and rewritten by the provider |
| Fresh for | `immutable_ttl` (`0` = forever) | `mutable_ttl` |
| After expiry | refetched from upstream | revalidated with `If-None-Match` when `check_mutable` is set and an ETag is stored, else refetched |
| Upstream down | `stale_on_error` serves the stored copy and re-arms the TTL; an open circuit breaker always serves stale if a copy exists, else 503 | same |

Concurrent misses on one path share a fetch lock. Waiters poll the store for
up to 5s instead of all going upstream.

## Eviction

```sh
curl -X DELETE https://artifactapi.example.com/api/v2/remotes/<name>/objects/<path>
curl -X DELETE https://artifactapi.example.com/api/v2/remotes/<name>/objects/<dir>/*
```

- Evict drops the artifact row, the index object and the Redis TTL and ETag keys, so the next request goes upstream whatever the class or TTL.
- Deleting a remote drops its Redis TTL and ETag keys, so a re-created remote of the same name starts cold.
- A single-path evict takes the fetch lock first, so an in-flight fetch can't re-cache the path. It returns 503 if the lock stays busy.
- A wildcard must end in `<dir>/*` and takes no lock.
- Returns 204, or 404 for an unknown remote.

## Examples

Bodies for `POST https://artifactapi.example.com/api/v2/remotes`.

GitHub release assets through a `generic` remote, allowlisted:

```json
{"name": "github", "package_type": "generic", "base_url": "https://github.com",
 "immutable_ttl": 0, "mutable_ttl": 7200,
 "patterns": ["^mikefarah/yq/releases/download/.*/yq_linux_amd64$"]}
```

rpm remote: `repodata/*` and `repomd.xml` are mutable by default, so only the TTLs are needed:

```json
{"name": "almalinux", "package_type": "rpm", "base_url": "https://repo.almalinux.org/almalinux",
 "immutable_ttl": 0, "mutable_ttl": 3600, "check_mutable": true, "stale_on_error": true}
```

Force a moving path mutable, e.g. branch tarballs on `generic`:

```json
{"mutable_patterns": ["/archive/refs/heads/.*\\.tar\\.gz$"], "mutable_ttl": 600}
```

## Built-in classification per type

Each type's page lists its auto-detected mutable and immutable paths:
[alpine](../alpine/remote.md), [alpine github](../alpine/remote-github.md),
[cargo](../cargo/remote.md), [deb](../deb/remote.md), [deb github](../deb/remote-github.md),
[docker](../docker/remote.md), [generic](../generic/remote.md), [gems](../gems/remote.md), [goproxy](../goproxy/remote.md),
[helm](../helm/remote.md), [npm](../npm/remote.md), [nuget](../nuget/remote.md),
[puppet](../puppet/remote.md), [pypi](../pypi/remote.md), [rpm](../rpm/remote.md),
[rpm github](../rpm/remote-github.md), [terraform](../terraform/remote.md).
