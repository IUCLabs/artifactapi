# Artifact Storage System

FastAPI caching proxy that downloads and stores files from remote sources in S3-compatible storage.

## Features

- Remote definitions via `remotes.yaml` — generic HTTP, Alpine APK, RPM, Docker, PyPI, npm, Helm
- Virtual repositories — merge multiple remotes of the same package type into a single unified index
- Immutable/mutable caching model with per-remote TTLs
- Conditional revalidation (`If-None-Match` / `If-Modified-Since`) on TTL expiry
- Stale-on-upstream-error: refreshes TTL when backend is unreachable rather than evicting
- URL rewriting for PyPI simple index, npm metadata, and Helm `index.yaml`
- Access control via regex patterns — unmatched paths return 403

## Architecture

```
client → /api/v1/remote/{remote}/{path}
           ↓
         Redis: mutable TTL check
           ↓ miss / expired
         S3: object exists?
           ↓ no
         upstream remote → S3 + PostgreSQL metadata
           ↓
         response (X-Artifact-Source: cache|remote)
```

Docker Registry traffic uses the `/v2/{remote}/{path}` endpoint implementing the Docker Registry HTTP API v2.

### Code layout

```
src/artifactapi/
├── main.py               — FastAPI app + thin route declarations only
├── config.py             — ConfigManager (loads remotes.yaml)
├── metrics.py            — Prometheus + Redis metrics
├── docker_auth.py        — backwards-compat shim → auth/docker.py
├── artifact/             — route handler implementations
│   ├── proxy.py          — GET /api/v1/remote (remote proxy, cache, revalidation)
│   ├── virtual.py        — GET /api/v1/virtual (virtual repo index merging)
│   ├── local.py          — PUT/HEAD/DELETE /api/v1/remote (local repos)
│   ├── docker.py         — /v2/ Docker Registry v2 proxy
│   ├── discovery.py      — /api/v1/artifacts discovery + bulk cache
│   └── flush.py          — PUT /cache/flush
├── auth/
│   ├── __init__.py       — re-exports Docker auth helpers
│   └── docker.py         — Bearer token fetching + in-memory cache
├── cache/
│   ├── __init__.py       — re-exports RedisCache
│   └── redis.py          — RedisCache (TTL keys, ETag metadata)
├── database/
│   ├── __init__.py       — re-exports DatabaseManager
│   └── postgres.py       — DatabaseManager (artifact + local-file tables)
├── storage/
│   ├── __init__.py       — re-exports S3Storage
│   └── s3.py             — S3Storage (MinIO/S3 abstraction)
└── remote/
    ├── __init__.py
    ├── base.py           — content-type detection
    ├── generic.py        — generic HTTP remotes
    ├── helm.py           — Helm index.yaml URL rewriting
    ├── npm.py            — npm metadata URL rewriting
    ├── python.py         — PyPI URL construction + HTML rewriting
    └── rpm.py            — RPM remotes
```

## API Endpoints

| Method | Path | Description |
|---|---|---|
| `GET` | `/api/v1/remote/{remote}/{path}` | Fetch artifact (auto-cache on miss) |
| `GET` | `/api/v1/virtual/{virtual}/{path}` | Fetch from virtual (merged) repository |
| `GET` | `/api/v1/local/{local}/{path}` | Download from local repository |
| `PUT` | `/api/v1/local/{local}/{path}` | Upload to local repository |
| `HEAD` | `/api/v1/local/{local}/{path}` | Check existence (local) |
| `DELETE` | `/api/v1/local/{local}/{path}` | Delete from local repository |
| `GET` | `/v2/{remote}/{path}` | Docker Registry v2 proxy |
| `PUT` | `/cache/flush` | Flush cache entries |
| `GET` | `/health` | Health check |
| `GET` | `/config` | View loaded configuration |
| `GET` | `/` | API info and available remotes |

## Configuration

Runtime settings come from environment variables; remote definitions live in one or more YAML files pointed to by `CONFIG_PATH`.

### Environment Variables

| Variable | Description |
|---|---|
| `CONFIG_PATH` | Path to a config YAML file **or** a directory of YAML files |
| `DBHOST`, `DBPORT`, `DBUSER`, `DBPASS`, `DBNAME` | PostgreSQL connection |
| `REDIS_URL` | Redis URL (e.g. `redis://localhost:6379`) |
| `MINIO_ENDPOINT` | MinIO/S3 endpoint |
| `MINIO_ACCESS_KEY` | S3 access key |
| `MINIO_SECRET_KEY` | S3 secret key |
| `MINIO_BUCKET` | S3 bucket name |
| `MINIO_SECURE` | Use HTTPS (`true`/`false`) |

### Split configuration

`CONFIG_PATH` accepts three forms:

**Single file** (original behaviour):
```
CONFIG_PATH=/etc/artifactapi/remotes.yaml
```

**Directory** — all `*.yaml` / `*.yml` files in the directory are loaded and merged alphabetically. `remotes` keys are merged across files; later files win on conflict:
```
CONFIG_PATH=/etc/artifactapi/conf.d/
```

**Main file + `config_dir`** — the main file holds global settings and a `config_dir` pointer; each file in that directory contributes its own `remotes`. Relative `config_dir` paths are resolved relative to the main file:
```yaml
# /etc/artifactapi/config.yaml
config_dir: conf.d   # or an absolute path

# s3/redis/database settings go here (or in env vars)
remotes: {}          # optional base remotes
```

### Configuration structure

Repositories are declared under three top-level keys matching their type:

```yaml
remotes:            # proxy (caching) remotes
  remote-name:
    base_url: "https://example.com"
    package: "generic"        # generic, alpine, rpm, docker, pypi, npm, helm
    description: "..."
    immutable_patterns:       # regex — cached forever
      - ".*\\.tar\\.gz$"
    mutable_patterns:         # regex — expire after mutable_ttl
      - "index\\.yaml$"
    check_mutable_updates: false   # send HEAD (If-None-Match) on TTL expiry
    cache:
      immutable_ttl: 0        # 0 = indefinitely
      mutable_ttl: 3600

virtuals:           # virtual (merged-index) repositories
  virtual-name:
    package: "helm"
    members:
      - remote-a
      - remote-b

locals:             # local upload repositories (no base_url)
  local-name:
    package: "generic"
    cache:
      immutable_ttl: 0
      mutable_ttl: 0
```

## Remote Types

### generic

Arbitrary HTTP file servers — GitHub releases, HashiCorp, custom servers.

```yaml
remotes:
  github:
    base_url: "https://github.com"
    package: "generic"
    immutable_patterns:
      - "gruntwork-io/terragrunt/.*terragrunt_linux_amd64.*"
    cache:
      immutable_ttl: 0

  github-archive:
    base_url: "https://github.com"
    package: "generic"
    immutable_patterns:
      - ".*/archive/refs/tags/.*\\.tar\\.gz$"   # tag archives never change
    mutable_patterns:
      - ".*/archive/refs/heads/main\\.tar\\.gz$"  # branch archives can change
    check_mutable_updates: true
    cache:
      immutable_ttl: 0
      mutable_ttl: 86400
```

Access: `GET /api/v1/remote/github/owner/repo/releases/download/v1.0/binary.tar.gz`

### alpine

```yaml
remotes:
  alpine:
    base_url: "https://dl-cdn.alpinelinux.org"
    package: "alpine"
    immutable_patterns:
      - ".*/x86_64/.*\\.apk$"
    cache:
      immutable_ttl: 0
      mutable_ttl: 7200
```

`APKINDEX.tar.gz` is a built-in mutable pattern — no `mutable_patterns` entry needed.

### rpm

```yaml
remotes:
  almalinux:
    base_url: "https://mirror.example.com/almalinux"
    package: "rpm"
    immutable_patterns:
      - ".*/x86_64/.*\\.rpm$"
      - ".*/noarch/.*\\.rpm$"
    cache:
      immutable_ttl: 0
      mutable_ttl: 7200
```

`repomd.xml` and `repodata/` metadata files are built-in mutable patterns.

### docker

```yaml
remotes:
  dockerhub:
    base_url: "https://registry-1.docker.io"
    package: "docker"
    # username / password optional for public images
    cache:
      immutable_ttl: 0
      mutable_ttl: 300

  ghcr:
    base_url: "https://ghcr.io"
    package: "docker"
    username: "your-github-username"
    password: "ghp_your_pat"   # read:packages scope
    cache:
      immutable_ttl: 0
      mutable_ttl: 300
```

Tag manifests and `/tags/list` are built-in mutable patterns. Digest-addressed blobs are immutable.

For RKE2/containerd, configure `/etc/rancher/rke2/registries.yaml`:

```yaml
mirrors:
  docker.io:
    endpoint:
      - "https://artifacts.example.com"
    rewrite:
      "^(.*)$": "dockerhub/$1"
  ghcr.io:
    endpoint:
      - "https://artifacts.example.com"
    rewrite:
      "^(.*)$": "ghcr/$1"
```

### pypi

```yaml
remotes:
  pypi:
    base_url: "https://files.pythonhosted.org"
    package: "pypi"
    check_mutable_updates: true
    immutable_patterns:
      - "packages/.*\\.whl$"
      - "packages/.*\\.whl\\.metadata$"
      - "packages/.*\\.tar\\.gz$"
      - "packages/.*\\.zip$"
    cache:
      immutable_ttl: 0
      mutable_ttl: 600
```

> **Note**: Simple index requests (`/simple/{package}/`) are always fetched from `https://pypi.org`, regardless of `base_url`. This is hardcoded — `base_url` only controls where package files are downloaded from. For self-hosted registries (Gitea, Nexus) where both index and files share the same host, set `base_url` to that host and the override does not apply.

URLs in simple index HTML are rewritten to route package file downloads back through the same remote.

Configure uv:

```toml
# /etc/uv/uv.toml or ~/.config/uv/uv.toml
[[index]]
url = "https://artifacts.example.com/api/v1/remote/pypi/simple"
default = true
```

### npm

```yaml
remotes:
  npm:
    base_url: "https://registry.npmjs.org"
    package: "npm"
    check_mutable_updates: true
    immutable_patterns:
      - "\.tgz$"
    mutable_patterns:
      - "^(?!.*\.tgz$).*"
    cache:
      immutable_ttl: 0
      mutable_ttl: 600
```

`dist.tarball` URLs in package metadata JSON are rewritten to route tarball downloads back through the same remote.

Configure npm / yarn / pnpm:

```ini
# .npmrc or ~/.npmrc
registry=https://artifacts.example.com/api/v1/remote/npm/
```

### helm

```yaml
remotes:
  hashicorp-helm:
    base_url: "https://helm.releases.hashicorp.com"
    package: "helm"
    check_mutable_updates: true
    immutable_patterns:
      - "\\.tgz$"
    cache:
      immutable_ttl: 0
      mutable_ttl: 3600
```

`index.yaml` is a built-in mutable pattern. Chart URLs inside `index.yaml` are rewritten to route tarball downloads back through the same remote.

Configure Helm:

```bash
helm repo add hashicorp https://artifacts.example.com/api/v1/remote/hashicorp-helm
helm repo update
```

### virtual

A virtual repository presents a single unified index built from multiple member remotes of the same package type. Clients configure one endpoint and get access to all member remotes transparently.

All members must share the same `package` type as the virtual repo. Currently supported package types: `helm`.

```yaml
remotes:
  helm-hashicorp:
    base_url: "https://helm.releases.hashicorp.com"
    package: "helm"
    immutable_patterns:
      - "\\.tgz$"
    cache:
      immutable_ttl: 0
      mutable_ttl: 3600

  helm-bitnami:
    base_url: "https://charts.bitnami.com/bitnami"
    package: "helm"
    immutable_patterns:
      - "\\.tgz$"
    cache:
      immutable_ttl: 0
      mutable_ttl: 3600

virtuals:
  helm-all:
    package: "helm"
    members:
      - helm-hashicorp   # listed first = highest priority
      - helm-bitnami
```

**How it works:**

1. A request for the package index triggers a parallel fetch of each member's index from S3 cache, falling back to upstream if not yet cached.
2. Member indexes are merged into a single index with URL rewriting so artifact download URLs continue to resolve through the individual member remote.
3. The merged index is cached in Redis with a TTL equal to the minimum `mutable_ttl` across all members.

**Priority / conflict resolution:**

When the same artifact name and version appears in more than one member, the member listed **first** in `members` wins. Subsequent members contribute only artifacts not already present.

**Partial failures:**

If a member is unreachable and has no cached index, it is skipped and a warning is logged. The merged index is still served from available members. If *no* members can be reached, the request returns `502`.

**Caching:**

The merged index is cached using `min(mutable_ttl)` across all members. Each member's raw index is cached in S3 under its own remote key; the virtual handler reuses those copies when available. On rebuild, each member's parsed index is also stored as a compact msgpack file (`index.msgpack`) alongside the raw YAML, eliminating the YAML parse cost on subsequent rebuilds.

**Helm example:**

```bash
helm repo add all https://artifacts.example.com/api/v1/virtual/helm-all
helm repo update
```

Chart tarball URLs in the merged `index.yaml` are rewritten to point at the individual member remote (e.g. `…/api/v1/remote/helm-hashicorp/vault-0.27.0.tgz`), so downloads bypass the virtual endpoint entirely.

### local

```yaml
locals:
  local-generic:
    package: "generic"
    description: "Local file repository"
    cache:
      immutable_ttl: 0
      mutable_ttl: 0
```

No `base_url`. Files are uploaded via `PUT /api/v1/local/{name}/{path}` and downloaded via `GET /api/v1/local/{name}/{path}`.

## Caching Model

### Immutable patterns

Files matching `immutable_patterns` are cached for `immutable_ttl` seconds (0 = indefinitely). Use for versioned release artifacts that never change once published.

**Access control**: only paths matching an immutable or mutable pattern are served; all others return 403. Omitting `immutable_patterns` entirely allows all paths from that remote.

### Mutable patterns

Files matching `mutable_patterns` expire after `mutable_ttl` seconds and are re-fetched on the next request. Mutable files are always served regardless of `immutable_patterns`.

Each package type has built-in defaults that are merged with any user-defined `mutable_patterns`:

| Package type | Built-in mutable patterns |
|---|---|
| `alpine` | `APKINDEX\.tar\.gz$` |
| `rpm` | `repomd\.xml$`, `repodata/` metadata variants, `Packages\.gz$` |
| `docker` | Tag manifests (non-digest refs), `/tags/list` |
| `pypi` | `simple/` (per-package and top-level index pages) |
| `helm` | `index\.yaml$` |
| `npm` | *(none built-in — define via `mutable_patterns`)* |
| `generic` | *(none)* |

### Conditional revalidation

Set `check_mutable_updates: true` to send `HEAD` with `If-None-Match` / `If-Modified-Since` on TTL expiry. A 304 response refreshes the TTL without re-downloading. Only applies to user-defined `mutable_patterns` — built-in patterns are always re-fetched unconditionally.

### Stale-on-upstream-error

When a mutable file expires and the upstream is unreachable (connection refused, DNS failure, timeout), the cached copy is kept and its TTL refreshed. HTTP error responses (4xx, 5xx) are not treated as network failures and proceed with normal expiry.

### Quarantine (supply-chain protection)

Set `quarantine_new: true` and `quarantine_days: N` on a remote to block immutable artifacts published within the last N days. Requests return `404` until the quarantine period expires, giving time to detect malicious packages before they are consumed.

```yaml
remotes:
  pypi:
    base_url: "https://files.pythonhosted.org"
    package: "pypi"
    quarantine_new: true
    quarantine_days: 3        # block packages published in the last 3 days
    immutable_patterns:
      - "packages/.*\\.whl$"
      - "packages/.*\\.tar\\.gz$"
    cache:
      immutable_ttl: 0
      mutable_ttl: 600
```

The upstream `Last-Modified` response header is used as the publish date proxy. Artifacts that have no `Last-Modified` header are allowed through (fail-open). Mutable files (index pages, tag manifests) are never quarantined.
