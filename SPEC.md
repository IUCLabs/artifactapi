# ArtifactAPI Specification

## Repository model

Every repository entry in `remotes.yaml` has two orthogonal fields:

| field | values | meaning |
|---|---|---|
| `type` | `local`, `remote`, `virtual` | repository kind — how the repo is served |
| `package` | `docker`, `rpm`, `alpine`, `generic` | package format — what protocol and caching rules to apply |

**type**

- `local` — files are uploaded directly to the API and stored in S3; no upstream.
- `remote` — proxies and caches content from an upstream URL (`base_url`).
- `virtual` — aggregates multiple repositories (not yet implemented).

**package**

- `docker` — upstream speaks the OCI Distribution API (Bearer auth, manifest/blob paths).
- `rpm` — upstream is an RPM repository; repodata files are index files.
- `alpine` — upstream is an Alpine APK repository; `APKINDEX.tar.gz` is an index file.
- `generic` — plain HTTP file download; no format-specific logic.

---

## Caching

Two cache classes determine retention:

| class | stored | TTL |
|---|---|---|
| **file** | S3 object, no Redis entry | `file_ttl` — `0` means indefinite |
| **index** | S3 object + Redis TTL key | `index_ttl` — when the Redis key expires the S3 object is deleted and re-fetched |

Index files are mutable metadata that must expire. File-class objects are treated as immutable and cached indefinitely (unless `file_ttl` is set).

---

## Docker package rules

### URL construction

Remote URLs are prefixed with `/v2/` for `package: docker` remotes:

```
{base_url}/v2/{path}
```

e.g. `library/nginx/manifests/latest` → `https://registry-1.docker.io/v2/library/nginx/manifests/latest`

### Authentication

Docker registries use Bearer token challenges. On a `401 Unauthorized` response, the API:

1. Parses the `WWW-Authenticate: Bearer` header for `realm`, `service`, and `scope`.
2. Fetches a token from the auth realm, supplying `username`/`password` from the remote config if present.
3. Retries the request with `Authorization: Bearer <token>`.

Tokens are cached in-memory keyed by `(realm, service, scope, username)` and expire 30 seconds before their stated `expires_in`.

### Cache classification

| path pattern | mutable | class | TTL source |
|---|---|---|---|
| `/manifests/<tag>` | yes | index | `index_ttl` |
| `/tags/list` | yes | index | `index_ttl` |
| `/manifests/sha256:<digest>` | no | file | `file_ttl` |
| `/blobs/sha256:<digest>` | no | file | `file_ttl` |

Tag-based manifests and tag lists are mutable and cached as index. Digest-pinned manifests and blobs are content-addressed and cached indefinitely as files.

### Blob deduplication

Blobs are stored under a digest-keyed path shared across all images on the same remote:

```
{remote_name}/blobs/sha256/{digest}
```

The same layer pulled by different images is stored once.

### Accept headers

| path | `Accept` header sent upstream |
|---|---|
| `/manifests/…` | `application/vnd.docker.distribution.manifest.v2+json`, `application/vnd.oci.image.manifest.v1+json`, `application/vnd.oci.image.index.v1+json`, `application/vnd.docker.distribution.manifest.list.v2+json` |
| `/blobs/…` | `application/octet-stream` |

---

## OCI Distribution API endpoint

The API exposes a native Docker registry interface so clients can use `docker pull` directly:

```
GET  /v2/                                   — version ping
GET  /v2/{remote}/{image}/manifests/{ref}   — fetch manifest
HEAD /v2/{remote}/{image}/manifests/{ref}   — manifest metadata
GET  /v2/{remote}/{image}/blobs/{digest}    — fetch blob
HEAD /v2/{remote}/{image}/blobs/{digest}    — blob metadata
```

Responses include `Docker-Distribution-Api-Version`, `Docker-Content-Digest`, and the correct OCI `Content-Type` (detected from the manifest `mediaType` field).

Only remotes with `package: docker` are accessible via this endpoint. All other remotes return `400`.

---

## include_patterns

`include_patterns` is a list of Python regexes applied to every request before any upstream fetch or cache lookup.

**Generic remotes (`/api/v1/remote/…`):**
- Patterns match against the file path and the full path.
- Index files (mutable metadata) bypass pattern checks and are always allowed.

**Docker remotes (`/v2/…`):**
- Patterns match against the image name (first two path segments, e.g. `library/nginx`) and the full path.
- The index-file exemption does **not** apply — patterns restrict whole images, including their manifests and tag lists.
- No patterns configured → all images allowed.

Returns `403` when a request is blocked.

---

## Versioning

The package version is derived from git tags via `hatch-vcs`. Tags follow the format `v{MAJOR}.{MINOR}.{PATCH}`.

Docker images are built with the version injected at build time:

```
SETUPTOOLS_SCM_PRETEND_VERSION=<version> uv sync --frozen
```

The `Makefile` provides `patch`, `minor`, and `major` targets that tag the current commit and rebuild the container image.
