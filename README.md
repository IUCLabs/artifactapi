# ArtifactAPI

Caching proxy for package repositories. Single Go binary, 11 package types, content-addressable storage, managed by Terraform.

## Quick Start

```bash
# Start backing services
docker compose up -d postgres redis minio

# Build and run
make build
./bin/artifactapi

# Frontend (separate container or dev server)
cd ui && npm install && npm run dev
```

API: `http://localhost:8000` | Frontend: `http://localhost:5173`

## Package Types

| Type | Mutable (auto-detected) | Immutable (auto-detected) |
|---|---|---|
| `generic` | nothing | everything |
| `docker` | tag manifests, `/tags/list` | blobs, digest manifests |
| `helm` | `index.yaml` | `.tgz` charts |
| `pypi` | `simple/*` index pages | `.whl`, `.tar.gz` |
| `npm` | package metadata | `.tgz` tarballs |
| `rpm` | `repomd.xml`, `repodata/*` | `.rpm` |
| `alpine` | `APKINDEX.tar.gz` | `.apk` |
| `puppet` | `v3/modules/*`, `v3/releases*` | `.tar.gz` |
| `terraform` | `*/versions` | `*/download/*/*` |
| `goproxy` | `@v/list`, `@latest` | `.info`, `.mod`, `.zip` |
| `cargo` | sparse index files (`config.json` synthesized) | `crates/*/*.crate` |
| `github_rpm` | `repodata/*` (synthesized) | `.rpm` (redirected) |

Providers classify paths automatically. Users only configure what to proxy and TTLs.

### `github_rpm` — GitHub releases as a yum repo (metadata-only, no precache)

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

#### Background syncer

A single process-wide **background syncer** keeps every `github_rpm` remote's
derived metadata current off the client request path:

- **Prime on create.** Creating a `github_rpm` remote enqueues a background prime
  scan, so its metadata is derived right away without blocking the create call.
  The first `dnf` request is served from cache. If a request arrives before the
  prime lands, it returns a retryable `503` (with `Retry-After`) rather than
  serving an empty repo or blocking on a multi-minute derive.
- **Periodic re-check, driven by `mutable_ttl`.** Each remote is re-checked for
  new or changed releases no more often than its `mutable_ttl`. New/changed
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
  ~once per `mutable_ttl` regardless of replica count, and the shared `etag`
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

  # Optional: restrict which release assets become packages (regex on filename).
  patterns = [".*\\.x86_64\\.rpm$", ".*\\.noarch\\.rpm$"]

  # Optional: a token for private repos / higher API rate limits.
  # password = "ghp_..."
}
```

`dnf` config: `baseurl=https://artifactapi.example/api/v1/remote/acme-tools`.
The repo is multi-arch (no `$basearch` needed) — `dnf` selects matching packages
from the synthesized metadata.

### GitHub authentication

Anonymous GitHub is capped at **60 requests/hour** and cannot read private
repositories. Configure a **server-level GitHub credential** to raise the ceiling
to roughly **5000 requests/hour** and to read private-repo release assets. The
credential is a process-wide machine identity applied by default to *every*
outbound GitHub request — the releases scan, the ranged asset-header fetches, and
the generic-github byte proxy that streams private release assets.

The credential is read from the environment (deliver it from a Vault or
Kubernetes secret). It is **never** stored per-remote in the database, **never**
returned by any API, and **never** logged. Configure **exactly one** mode.

**Precedence.** A remote's own `username`/`password` credential still wins for
that remote's requests; the server credential is the default for everything else.
With no credential configured at all, requests stay anonymous (current behavior).
Partial configuration (e.g. an App id with no private key) is a **startup error**
— artifactapi fails closed rather than silently falling back to anonymous.

Both modes share the syncer's single global rate limiter, so a token simply
raises the effective GitHub ceiling; the default limiter settings stay safe.

#### Mode 1 — Personal Access Token (minimum viable, recommended for free accounts)

Set `GITHUB_TOKEN`. It is sent as `Authorization: Bearer <token>`.

Recommended free-account setup — a **fine-grained PAT** scoped to just the target
repositories:

1. GitHub → *Settings → Developer settings → Personal access tokens →
   Fine-grained tokens → Generate new token*.
2. Limit *Repository access* to the specific repo(s) serving releases.
3. Grant repository permissions **Contents: Read-only** and **Metadata:
   Read-only** (Metadata is mandatory and auto-selected).

A classic PAT with the `repo` scope also works but is broader than necessary.

```bash
GITHUB_TOKEN=github_pat_xxxxxxxx
```

#### Mode 2 — GitHub App installation token (proper machine identity)

A GitHub App is not tied to a personal account and can be created and installed on
free personal repos. artifactapi mints a short-lived RS256 **JWT** from the app
private key, exchanges it at `POST /app/installations/{id}/access_tokens` for a
~1-hour **installation access token**, caches that token, and refreshes it a few
minutes before expiry (thread-safe, single-flighted).

1. GitHub → *Settings → Developer settings → GitHub Apps → New GitHub App*.
2. Under *Permissions → Repository permissions* grant **Contents: Read-only**
   (Metadata: Read-only is implied).
3. Generate a **private key** (downloads a PEM) and note the **App ID**.
4. *Install* the App on the account and select the target repositories, then read
   the **Installation ID** from the installation URL
   (`.../settings/installations/<installation-id>`).

```bash
GITHUB_APP_ID=123456
GITHUB_APP_INSTALLATION_ID=7654321
GITHUB_APP_PRIVATE_KEY_PATH=/etc/artifactapi/github-app.pem
# or inline PEM (e.g. mounted from a secret):
# GITHUB_APP_PRIVATE_KEY="-----BEGIN RSA PRIVATE KEY-----\n...\n-----END RSA PRIVATE KEY-----"
```

## Terraform

Remotes and virtuals are managed by Terraform. Each package type has its own resource:

```hcl
resource "artifactapi_remote_generic" "github" {
  name     = "github"
  base_url = "https://github.com"

  immutable_ttl = 0
  mutable_ttl   = 7200

  patterns = [
    "ducaale/xh/.*/xh-.*-x86_64-unknown-linux-musl.tar.gz$",
    "mikefarah/yq/.*/yq_linux_amd64$",
  ]

  mutable_patterns = [
    ".*/archive/refs/heads/.*\\.tar\\.gz$",
  ]
}

resource "artifactapi_remote_docker" "dockerhub" {
  name     = "dockerhub"
  base_url = "https://registry-1.docker.io"

  immutable_ttl    = 0
  mutable_ttl      = 300
  ban_tags_enabled = true
  ban_tags         = ["latest"]

  patterns = [
    "^library/postgres",
    "^library/redis",
  ]
}

resource "artifactapi_remote_helm" "jetstack" {
  name     = "jetstack"
  base_url = "https://charts.jetstack.io"

  immutable_ttl = 0
  mutable_ttl   = 3600
}

resource "artifactapi_virtual" "helm" {
  name         = "helm"
  package_type = "helm"
  members      = [artifactapi_remote_helm.jetstack.name]
}
```

Provider: [terraform-provider-artifactapi](../terraform-provider-artifactapi)

### Serving providers as a registry

A local `terraform` repo is a real provider registry: upload
`terraform-provider-{type}_{version}_{os}_{arch}.zip` files under
`{namespace}/{type}/`, and Terraform installs them from a bare source address —
no `.terraformrc` mirror config:

```hcl
terraform {
  required_providers {
    artifactapi = {
      source  = "artifactapi.example.com/<repo>/<type>"
      version = "0.1.2"
    }
  }
}
```

The Terraform *namespace* segment is the artifactapi repo name; the provider is
matched by *type*. The registry serves service discovery
(`/.well-known/terraform.json`), the `providers.v1` version/download endpoints,
and a GPG-signed `SHA256SUMS` per the provider registry protocol.

Signing needs a GPG key. By default artifactapi generates one on first start and
stores it in the database (`signing_keys` table), so every replica shares it and
there's nothing to provision. To bring your own key instead, point
`TF_SIGNING_KEY_PATH` at an armored private key (optionally
`TF_SIGNING_KEY_PASSPHRASE`), which takes precedence over the generated one.
`TF_PROVIDER_PROTOCOLS` (default `5.0,6.0`) sets the advertised plugin protocols.

### Local docker registry

A local `docker` repo is a real container registry, not a mirror: it serves the
Docker Registry HTTP API V2 for both push and pull, so any client (`docker`,
`podman`, `skopeo`, `buildah`) can use it directly.

```sh
docker tag myapp:latest artifactapi.example.com/docker-internal/myapp:latest
docker push        artifactapi.example.com/docker-internal/myapp:latest
docker pull        artifactapi.example.com/docker-internal/myapp:latest
```

The first path segment after `/v2/` is the artifactapi repo name; the remainder
is the image name. Blobs and manifests are stored through the shared
content-addressable store (deduplicated by digest, reaped by GC once
unreferenced); tags are mutable references and re-pushing a tag moves it. Blob
uploads support both the monolithic and chunked (`POST`/`PATCH`/`PUT`) flows.

## Access Control

| Field | Default | Behaviour |
|---|---|---|
| `patterns` | empty (proxy all) | If set, only matching paths are proxied. Acts as allowlist. |
| `blocklist` | empty | Matching paths always denied. Checked first. |
| `mutable_patterns` | empty | Override: force paths to mutable TTL. |
| `immutable_patterns` | empty | Override: force paths to immutable TTL. |

No patterns + no blocklist = open proxy. Provider handles mutability classification automatically.

## API

### Proxy (v1)

```
GET /api/v1/remote/{name}/{path}     Proxy/cache artifact
GET /api/v1/virtual/{name}/{path}    Virtual repo (merged index)
GET /v2/{name}/{path}                Docker Registry v2
```

### Management (v2)

```
GET/POST        /api/v2/remotes              List / create remotes
GET/PUT/DELETE  /api/v2/remotes/{name}       Read / update / delete remote
GET/DELETE      /api/v2/remotes/{name}/objects  Browse / evict cached objects
GET             /api/v2/stats                Overview stats
GET             /api/v2/health               Service health
POST            /api/v2/probe                Test a remote (fetch without streaming to client)
GET             /api/v2/events               SSE event stream
```

## Architecture

```
PostgreSQL  ─── config (remotes, virtuals), artifact metadata, access log
Redis       ─── TTL keys, fetch locks, circuit breaker state
S3/MinIO    ─── content-addressable blob storage (blobs/sha256/{hash})
```

S3 client supports MinIO, Ceph RGW, and AWS S3 (via minio-go).

### Schema migrations

The SQL schema lives in `migrations/` as numbered `.sql` files, embedded into
the binary and applied at startup before the server listens, so there is no
mirrored copy of the schema in the deployment to drift out of sync.

The runner is [`golib/pg`](https://github.com/IUCLabs/golib)'s `pg.NewMigrated`
— artifactapi owns the SQL, the shared library owns the mechanics.

Each start takes `pg_advisory_lock` on a fixed key (FNV-1a/64 of the lock name
`artifactapi-migrations`), creates `schema_migrations` (`version`, `applied_at`)
if missing, and applies every embedded file whose filename is not yet recorded —
in lexical (version) order, each file's SQL and its tracking row in one
transaction — then unlocks. Replicas starting together queue on the lock and
then find nothing to do.

A file absent from `schema_migrations` is re-run even when the database already
has the schema, which is how a database migrated by the old untracked inline DDL
picks `0001_init.sql` up: the statements are `IF NOT EXISTS`-guarded, so the
re-run is a no-op that only lands the tracking row. New migrations must stay
additive and idempotent for the same reason; a test enforces it.

## Environment Variables

| Variable | Default | Description |
|---|---|---|
| `LISTEN_ADDR` | `:8000` | Server listen address |
| `DBHOST` | `localhost` | PostgreSQL host |
| `DBPORT` | `5432` | PostgreSQL port |
| `DBUSER` | `artifacts` | PostgreSQL user |
| `DBPASS` | | PostgreSQL password |
| `DBNAME` | `artifacts` | PostgreSQL database |
| `DBSSL` | `disable` | PostgreSQL `sslmode` |
| `REDIS_URL` | `redis://localhost:6379` | Redis URL |
| `MINIO_ENDPOINT` | `localhost:9000` | S3 endpoint |
| `MINIO_ACCESS_KEY` | | S3 access key |
| `MINIO_SECRET_KEY` | | S3 secret key |
| `MINIO_BUCKET` | `artifacts` | S3 bucket |
| `MINIO_SECURE` | `false` | Use HTTPS for S3 |
| `MINIO_REGION` | | S3 region (AWS) |
| `GITHUB_SYNC_RATE` | `1` | `github_rpm` syncer global GitHub request rate (req/s), shared across all remotes. `1`/s = 3600/hr, under an authenticated token's ~5000/hr; unauthenticated (~60/hr) relies on ETag/304 |
| `GITHUB_SYNC_BURST` | `5` | Token-bucket burst for the shared limiter |
| `GITHUB_SYNC_WORKERS` | `3` | Concurrent `github_rpm` scan workers |
| `GITHUB_SYNC_POLL_INTERVAL` | `60` | Base scheduler tick in seconds; per-remote cadence is its `mutable_ttl`, enforced by the DB lease |
| `GITHUB_TOKEN` | | Server-level GitHub PAT (fine-grained or classic), sent as `Authorization: Bearer`. Applies to every GitHub request; per-remote creds override it. See [GitHub authentication](#github-authentication) |
| `GITHUB_APP_ID` | | GitHub App id (App auth mode; mutually exclusive with `GITHUB_TOKEN`) |
| `GITHUB_APP_INSTALLATION_ID` | | GitHub App installation id |
| `GITHUB_APP_PRIVATE_KEY` | | GitHub App private key, inline PEM |
| `GITHUB_APP_PRIVATE_KEY_PATH` | | GitHub App private key, file path (alternative to inline PEM) |
| `ARTIFACTAPI_OIDC_ISSUER` | | OIDC issuer URL (e.g. Vault `https://vault.example.com:8200/v1/identity/oidc`). When set, `POST /api/v2/remotes`, `POST /api/v2/virtuals` and `PUT`/`DELETE` on `/api/v2/{remotes,virtuals}/{name}` require `Authorization: Bearer <jwt>` verified against the issuer's JWKS. Uploads, object/file deletes, probe, reads and `/v2` stay open. Unset = no auth |
| `ARTIFACTAPI_OIDC_AUDIENCE` | | Comma-separated allowed `aud` values (e.g. the Vault OIDC role's client_id). Required when `ARTIFACTAPI_OIDC_ISSUER` is set |

## Development

```bash
make build       # Build binary
make test        # Unit tests
make e2e         # E2E tests (needs Docker)
make lint        # golangci-lint + go vet
make fmt         # gofmt + goimports
```

### `GOPRIVATE`

artifactapi depends on `github.com/IUCLabs/golib`, which is served by Gitea and
is unknown to `proxy.golang.org` / `sum.golang.org`. Module resolution therefore
needs:

```
export GOPRIVATE=git.example.com
```

The `Makefile` exports it for every target, and the `Dockerfile` and the
woodpecker Go steps set it themselves, so `make build|test|lint` and CI work on
a clean checkout. Only bare `go` commands run outside `make` need it in your
shell — set it there (or in your shell profile) rather than with `go env -w`,
which is machine state this repo cannot carry.

### TUI

```bash
./bin/artifactapi tui --endpoint http://localhost:8000
```
