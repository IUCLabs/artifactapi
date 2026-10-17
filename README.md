# ArtifactAPI

Caching proxy for package repositories. Single Go binary, 12 package types, content-addressable storage, managed by Terraform.

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

Providers classify paths automatically. Users only configure what to proxy and TTLs.

Classification precedence, TTLs and eviction for every remote: [docs/common/caching.md](docs/common/caching.md).

Each type's paths, Terraform examples and local/virtual behaviour are in `docs/<type>/`:

- `alpine`: [remote](docs/alpine/remote.md), [remote-github](docs/alpine/remote-github.md)
- `cargo`: [remote](docs/cargo/remote.md)
- `deb`: [remote](docs/deb/remote.md), [remote-github](docs/deb/remote-github.md)
- `docker`: [remote](docs/docker/remote.md), [local](docs/docker/local.md)
- `generic`: [remote](docs/generic/remote.md)
- `gems`: [remote](docs/gems/remote.md)
- `goproxy`: [remote](docs/goproxy/remote.md)
- `helm`: [remote](docs/helm/remote.md), [local](docs/helm/local.md), [virtual](docs/helm/virtual.md)
- `npm`: [remote](docs/npm/remote.md), [local](docs/npm/local.md), [virtual](docs/npm/virtual.md)
- `nuget`: [remote](docs/nuget/remote.md)
- `puppet`: [remote](docs/puppet/remote.md), [local](docs/puppet/local.md)
- `pypi`: [remote](docs/pypi/remote.md)
- `rpm`: [remote](docs/rpm/remote.md), [remote-github](docs/rpm/remote-github.md)
- `terraform`: [remote](docs/terraform/remote.md), [local](docs/terraform/local.md)

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

Remotes and virtuals are managed by Terraform. Each package type has its own resource; see `docs/<type>/` for examples.

Provider: [terraform-provider-artifactapi](https://git.example.com/example/terraform-provider-artifactapi)

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
PUT /api/v1/local/{name}/{package}   npm publish (local npm repos)
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
