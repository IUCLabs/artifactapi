# ArtifactAPI v3 — Go Rewrite Plan

## Context

ArtifactAPI is a production artifact proxy/cache serving ~42 remotes (Docker registries, Helm repos, RPM/Alpine repos, GitHub releases, PyPI, npm, Puppet Forge, Terraform registries, Go module proxies) across a Kubernetes cluster. The current Python (FastAPI) implementation works but has architectural debt: opaque hashed S3 paths, no UI for visibility, YAML config files that drift, no garbage collection, no access logging, and virtual repos limited to Helm only.

The v3 rewrite targets: a single Go binary (API + TUI), a separate React frontend (own Dockerfile), a Terraform provider (separate repo), content-addressable storage, and a cleaner data model that makes the cache inspectable and manageable.

**Repo**: Same repo (`github.com/IUCLabs/artifactapi`), new branch.
**Module**: `github.com/IUCLabs/artifactapi`
**Frontend**: React + Vite, separate Dockerfile, talks to API
**Terraform provider**: Separate repo (`terraform-provider-artifactapi`)

---

## Architecture Overview

```
┌───────────────────────────────────┐    ┌──────────────────────┐
│        Go Binary (API + TUI)      │    │   Frontend Container │
│                                   │    │                      │
│  ┌──────────┐  ┌───────────────┐  │    │  React + Vite SPA    │
│  │ REST API │  │ Proxy Engine  │  │◄───│  nginx / node serve  │
│  │ /api/v2  │  │ /api/v1/...   │  │    │  Dockerfile.ui       │
│  │          │  │ /v2/... (OCI) │  │    └──────────────────────┘
│  └────┬─────┘  └──────┬────────┘  │
│       │               │           │    ┌──────────────────────┐
│  ┌────┴───────────────┴────────┐  │    │ Terraform Provider   │
│  │        Data Layer           │  │◄───│ (separate repo)      │
│  │  PostgreSQL · Redis · S3    │  │    └──────────────────────┘
│  └─────────────────────────────┘  │
│                                   │    ┌──────────────────────┐
│  ┌─────────────────────────────┐  │    │ TUI (subcommand)     │
│  │  artifactapi tui            │──│───►│ artifactapi tui      │
│  └─────────────────────────────┘  │    │ --endpoint <url>     │
└───────────────────────────────────┘    └──────────────────────┘
```

Three independent deployment units:
1. **Go binary** — API server + TUI subcommand (single `Dockerfile`)
2. **React frontend** — SPA served by nginx (`Dockerfile.ui`), talks to `/api/v2`
3. **Terraform provider** — separate repo, calls `/api/v2` CRUD

---

## Project Structure (Modular)

```
artifactapi/
├── cmd/
│   └── artifactapi/
│       └── main.go                     # entrypoint: serve / tui subcommands
│
├── pkg/                                # PUBLIC — importable by terraform provider, CLI tools
│   ├── models/                         # shared domain types
│   │   ├── remote.go                   # Remote, RemoteConfig, PackageType enum
│   │   ├── virtual.go                  # Virtual, VirtualConfig
│   │   ├── artifact.go                 # Artifact, Blob, AccessLogEntry
│   │   ├── local.go                    # LocalFile, LocalRepo
│   │   └── stats.go                    # RemoteStats, OverviewStats
│   └── client/                         # typed Go API client (used by TUI + Terraform provider)
│       ├── client.go                   # Client struct, base HTTP
│       ├── remotes.go                  # remote CRUD methods
│       ├── virtuals.go                 # virtual CRUD methods
│       ├── objects.go                  # object browse/evict methods
│       └── stats.go                    # stats methods
│
├── internal/                           # PRIVATE — server internals
│   ├── server/
│   │   ├── server.go                   # HTTP server setup, router
│   │   └── middleware.go               # logging, recovery, request-id, access logging
│   │
│   ├── api/
│   │   ├── v1/                         # proxy endpoints (v1 compat)
│   │   │   ├── proxy.go               # GET /api/v1/remote/{name}/{path}
│   │   │   ├── docker.go              # /v2/{name}/{path}
│   │   │   ├── virtual.go             # GET /api/v1/virtual/{name}/{path}
│   │   │   └── local.go               # CRUD /api/v1/local/{name}/{path}
│   │   └── v2/                         # management API
│   │       ├── remotes.go             # CRUD + stats
│   │       ├── virtuals.go            # CRUD
│   │       ├── objects.go             # browse/evict cached objects
│   │       ├── stats.go               # overview, top-remotes
│   │       ├── events.go              # SSE stream
│   │       └── health.go              # health, metrics
│   │
│   ├── provider/                       # package-type providers (registry protocol handlers)
│   │   ├── provider.go                # Provider interface + registry
│   │   ├── generic/
│   │   │   ├── generic.go
│   │   │   └── generic_test.go
│   │   ├── docker/
│   │   │   ├── docker.go              # OCI Distribution v2 via go-containerregistry
│   │   │   ├── auth.go                # Bearer token fetch + cache
│   │   │   └── docker_test.go
│   │   ├── helm/
│   │   │   ├── helm.go                # index rewriting via helm.sh/helm/v3/pkg/repo
│   │   │   ├── merger.go              # virtual index merge
│   │   │   └── helm_test.go
│   │   ├── pypi/
│   │   │   ├── pypi.go                # simple index HTML rewriting
│   │   │   ├── merger.go              # virtual simple index merge
│   │   │   └── pypi_test.go
│   │   ├── npm/
│   │   │   ├── npm.go                 # metadata JSON rewriting
│   │   │   └── npm_test.go
│   │   ├── rpm/
│   │   │   ├── rpm.go                 # repodata patterns
│   │   │   └── rpm_test.go
│   │   ├── alpine/
│   │   │   ├── alpine.go              # APKINDEX patterns
│   │   │   └── alpine_test.go
│   │   ├── puppet/
│   │   │   ├── puppet.go              # file_uri JSON rewriting
│   │   │   └── puppet_test.go
│   │   ├── terraform/
│   │   │   ├── terraform.go           # registry protocol, download URL rewriting
│   │   │   └── terraform_test.go
│   │   └── goproxy/
│   │       ├── goproxy.go             # Go module proxy protocol (GOPROXY)
│   │       └── goproxy_test.go
│   │
│   ├── proxy/
│   │   ├── engine.go                  # core fetch-or-cache logic
│   │   ├── engine_test.go
│   │   ├── classifier.go              # immutable vs mutable classification
│   │   ├── classifier_test.go
│   │   ├── revalidator.go             # conditional HEAD requests (ETag/Last-Modified)
│   │   └── circuit.go                 # per-remote circuit breaker
│   │
│   ├── storage/
│   │   ├── s3.go                      # S3 client (minio-go — works with MinIO, Ceph, AWS)
│   │   ├── s3_test.go
│   │   ├── cas.go                     # content-addressable store logic
│   │   └── cas_test.go
│   │
│   ├── cache/
│   │   ├── redis.go                   # TTL management, fetch locks
│   │   ├── redis_test.go
│   │   └── lock.go                    # distributed lock abstraction
│   │
│   ├── database/
│   │   ├── postgres.go                # connection pool, migration runner
│   │   ├── queries/                   # SQL query files or sqlc-generated code
│   │   │   ├── remotes.sql.go
│   │   │   ├── virtuals.sql.go
│   │   │   ├── artifacts.sql.go
│   │   │   └── access_log.sql.go
│   │   └── migrations/               # golang-migrate SQL files
│   │       ├── 001_initial.up.sql
│   │       └── 001_initial.down.sql
│   │
│   ├── metrics/
│   │   └── prometheus.go             # counters, gauges, histograms
│   │
│   ├── gc/
│   │   ├── gc.go                      # background garbage collection goroutine
│   │   └── gc_test.go
│   │
│   ├── tui/
│   │   ├── app.go                     # Bubble Tea main model
│   │   ├── views/
│   │   │   ├── dashboard.go
│   │   │   ├── remotes.go
│   │   │   ├── objects.go
│   │   │   └── virtuals.go
│   │   └── components/
│   │       ├── table.go
│   │       └── statusbar.go
│   │
│   └── config/
│       └── env.go                     # environment variable parsing + validation
│
├── ui/                                 # React frontend — SEPARATE DOCKERFILE
│   ├── src/
│   │   ├── App.tsx
│   │   ├── pages/
│   │   │   ├── Dashboard.tsx
│   │   │   ├── Remotes.tsx
│   │   │   ├── RemoteDetail.tsx
│   │   │   ├── Virtuals.tsx
│   │   │   └── Objects.tsx
│   │   ├── components/
│   │   │   ├── RemoteTable.tsx
│   │   │   ├── ObjectBrowser.tsx
│   │   │   ├── StatsCard.tsx
│   │   │   └── EventFeed.tsx
│   │   └── api/
│   │       └── client.ts              # typed API client
│   ├── package.json
│   ├── vite.config.ts
│   ├── tsconfig.json
│   ├── Dockerfile.ui                  # multi-stage: node build → nginx
│   └── nginx.conf                     # proxy /api/* to backend, serve SPA
│
├── e2e/                                # end-to-end integration tests
│   ├── e2e_test.go                    # TestMain spins up docker-compose stack
│   ├── proxy_test.go                  # proxy through real remotes
│   ├── docker_test.go                 # Docker v2 protocol e2e
│   ├── management_test.go            # v2 API CRUD
│   ├── virtual_test.go               # virtual repo merge e2e
│   └── docker-compose.e2e.yml        # postgres + redis + minio for tests
│
├── go.mod
├── go.sum
├── Makefile
├── Dockerfile                          # Go binary (API server + TUI)
├── Dockerfile.ui                       # symlink or copy → ui/Dockerfile.ui
└── docker-compose.yml
```

### Key Modularisation Decisions

- **`pkg/models/`** — Shared domain types importable by the Terraform provider and any external tooling. No dependencies on internal packages
- **`pkg/client/`** — Typed Go API client used by both the TUI and the Terraform provider. Depends only on `pkg/models/` and stdlib
- **`internal/provider/`** — Each package type is its own subpackage with isolated tests. A provider registry maps `PackageType → Provider`
- **`internal/database/queries/`** — Use [sqlc](https://sqlc.dev/) to generate type-safe query functions from SQL, or hand-written query files
- **`e2e/`** — Separate test binary that spins up a real docker-compose stack

---

## Go Ecosystem Libraries

Prefer existing, maintained Go modules over writing protocol handlers from scratch.

### Package-Type Libraries

| Package Type | Go Module | What It Gives Us |
|---|---|---|
| **Docker/OCI** | `github.com/google/go-containerregistry` | Full Registry v2/OCI client: manifest parsing, auth challenges, blob operations. `pkg/registry` can implement a v2 server. Reference: `github.com/regclient/regclient` |
| **Helm** | `helm.sh/helm/v3/pkg/repo` | Parse/generate `index.yaml`, `IndexFile`/`ChartVersion` types, URL entries. Used directly for merge |
| **Terraform** | `github.com/hashicorp/terraform-registry-address` | Provider/module address parsing, `ForRegistryProtocol()` URL generation. Protocol spec: provider registry protocol v1 |
| **Go Modules** | `github.com/goproxy/goproxy` | Minimalist GOPROXY protocol handler, implements full spec as `http.Handler`. Handles `/@v/list`, `/@v/{v}.info`, `/@v/{v}.mod`, `/@v/{v}.zip`, `/@latest` |
| **RPM** | `rs3.io/go/rpm/repomd` | Parse `repomd.xml`, `primary.xml` with proper XML namespace handling |
| **Alpine** | `gitlab.alpinelinux.org/alpine/go` | Official Alpine library: parse APKINDEX, `.apk` files |
| **PyPI** | stdlib `golang.org/x/net/html` | No dedicated Go PyPI library exists. Parse simple index HTML with `x/net/html`, extract `<a>` tags. Minimal — the rewriting is just href replacement |
| **npm** | stdlib `encoding/json` | npm metadata is JSON — parse with stdlib, rewrite `dist.tarball` URLs. No special library needed |
| **Puppet Forge** | stdlib `encoding/json` | Forge API is JSON — parse and rewrite `file_uri` fields. Community lib `github.com/johnmccabe/go-puppetforge` exists but is thin; stdlib suffices |

### Infrastructure Libraries

| Purpose | Go Module | Why This One |
|---|---|---|
| **HTTP router** | `github.com/go-chi/chi/v5` | Lightweight, stdlib `http.Handler` compatible, middleware chain |
| **PostgreSQL** | `github.com/jackc/pgx/v5` | Pure Go, connection pooling, COPY support, prepared statements |
| **SQL generation** | `github.com/sqlc-dev/sqlc` | Generate type-safe Go from SQL queries — no ORM, no reflection |
| **Redis** | `github.com/redis/go-redis/v9` | Full Redis client, pipelining, pub/sub |
| **S3 (MinIO/Ceph/AWS)** | `github.com/minio/minio-go/v7` | Native S3-compatible client. Works with MinIO, Ceph RGW, AWS S3, any S3-compatible backend out of the box. Lighter than aws-sdk-go-v2, purpose-built for S3 compat |
| **DB migrations** | `github.com/golang-migrate/migrate/v4` | SQL file-based migrations, CLI + library |
| **Prometheus** | `github.com/prometheus/client_golang` | Counters, gauges, histograms |
| **TUI** | `github.com/charmbracelet/bubbletea` | Elm-architecture TUI framework |
| **TUI styling** | `github.com/charmbracelet/lipgloss` | Terminal styling |
| **TUI components** | `github.com/charmbracelet/bubbles` | Table, text input, spinner, etc. |
| **Structured logging** | `log/slog` (stdlib) | Go 1.21+ structured logging, zero dependencies |
| **Testing** | `github.com/stretchr/testify` | Assertions + require for unit tests |
| **Test containers** | `github.com/testcontainers/testcontainers-go` | Spin up Postgres/Redis/MinIO in e2e tests |

### S3 Client: Multi-Backend Support

Using `minio-go/v7` as the S3 client because it natively supports:
- **MinIO** — primary development/production target
- **Ceph RGW** — S3-compatible via endpoint config
- **AWS S3** — via region + credential config
- **Any S3-compatible** — GCS (interop mode), Wasabi, DigitalOcean Spaces, etc.

No abstraction layer needed — `minio-go` handles endpoint differences internally. Config:
```go
client, _ := minio.New(endpoint, &minio.Options{
    Creds:  credentials.NewStaticV4(accessKey, secretKey, ""),
    Secure: useTLS,
    Region: region, // optional, for AWS
})
```

---

## Data Layer

### PostgreSQL Schema

```sql
-- Remotes: managed exclusively by Terraform
CREATE TABLE remotes (
    name            TEXT PRIMARY KEY,
    package_type    TEXT NOT NULL,  -- generic, docker, helm, pypi, npm, rpm, alpine, puppet, terraform, goproxy
    base_url        TEXT NOT NULL,
    description     TEXT DEFAULT '',
    username        TEXT DEFAULT '',
    password        TEXT DEFAULT '',
    immutable_ttl   INTEGER DEFAULT 0,
    mutable_ttl     INTEGER DEFAULT 3600,
    check_mutable   BOOLEAN DEFAULT TRUE,
    immutable_patterns  TEXT[] DEFAULT '{}',  -- user-defined immutable patterns
    mutable_patterns    TEXT[] DEFAULT '{}',  -- user-defined mutable patterns (merged with provider built-ins)
    allowlist        TEXT[] DEFAULT '{}',     -- if empty, allow all paths; if non-empty, only matching paths proxied
    blocklist        TEXT[] DEFAULT '{}',     -- always denied, checked before allowlist
    ban_tags_enabled    BOOLEAN DEFAULT FALSE,
    ban_tags            TEXT[] DEFAULT '{}',
    quarantine_enabled  BOOLEAN DEFAULT FALSE,
    quarantine_days     INTEGER DEFAULT 3,
    stale_on_error      BOOLEAN DEFAULT TRUE,
    releases_remote     TEXT DEFAULT '',  -- terraform type: name of CDN remote for download URL rewriting
    managed_by          TEXT DEFAULT '',  -- 'terraform' or empty
    created_at      TIMESTAMPTZ DEFAULT NOW(),
    updated_at      TIMESTAMPTZ DEFAULT NOW()
);

-- Virtual repositories
CREATE TABLE virtuals (
    name            TEXT PRIMARY KEY,
    package_type    TEXT NOT NULL,
    description     TEXT DEFAULT '',
    members         TEXT[] NOT NULL,
    managed_by      TEXT DEFAULT '',
    created_at      TIMESTAMPTZ DEFAULT NOW(),
    updated_at      TIMESTAMPTZ DEFAULT NOW()
);

-- Content-addressable blob storage tracking
CREATE TABLE blobs (
    content_hash    TEXT PRIMARY KEY,
    s3_key          TEXT NOT NULL,
    size_bytes      BIGINT NOT NULL,
    content_type    TEXT DEFAULT 'application/octet-stream',
    created_at      TIMESTAMPTZ DEFAULT NOW()
);

-- Artifact metadata: maps (remote, path) → content blob
CREATE TABLE artifacts (
    id              BIGSERIAL PRIMARY KEY,
    remote_name     TEXT NOT NULL REFERENCES remotes(name) ON DELETE CASCADE,
    path            TEXT NOT NULL,
    content_hash    TEXT NOT NULL REFERENCES blobs(content_hash),
    upstream_etag   TEXT DEFAULT '',
    upstream_last_modified TIMESTAMPTZ,
    first_seen_at   TIMESTAMPTZ DEFAULT NOW(),
    last_fetched_at TIMESTAMPTZ DEFAULT NOW(),
    last_accessed_at TIMESTAMPTZ DEFAULT NOW(),
    fetch_count     BIGINT DEFAULT 1,
    access_count    BIGINT DEFAULT 1,
    UNIQUE(remote_name, path)
);

CREATE INDEX idx_artifacts_remote ON artifacts(remote_name);
CREATE INDEX idx_artifacts_last_accessed ON artifacts(last_accessed_at);

-- Local file uploads
CREATE TABLE local_files (
    id              BIGSERIAL PRIMARY KEY,
    repo_name       TEXT NOT NULL,
    file_path       TEXT NOT NULL,
    content_hash    TEXT NOT NULL REFERENCES blobs(content_hash),
    created_at      TIMESTAMPTZ DEFAULT NOW(),
    UNIQUE(repo_name, file_path)
);

-- Access log (append-only, powers dashboards)
CREATE TABLE access_log (
    id              BIGSERIAL PRIMARY KEY,
    remote_name     TEXT NOT NULL,
    path            TEXT NOT NULL,
    cache_hit       BOOLEAN NOT NULL,
    size_bytes      BIGINT DEFAULT 0,
    upstream_ms     INTEGER DEFAULT 0,
    client_ip       TEXT DEFAULT '',
    created_at      TIMESTAMPTZ DEFAULT NOW()
);

CREATE INDEX idx_access_log_remote_time ON access_log(remote_name, created_at);
```

### Redis Usage (Ephemeral Only)

| Key pattern | Type | TTL | Purpose |
|---|---|---|---|
| `ttl:{remote}:{path}` | STRING | remote's immutable/mutable TTL | Artifact freshness — existence = still fresh |
| `lock:{remote}:{path}` | STRING (NX) | 30s | Fetch lock — prevents thundering herd |
| `etag:{remote}:{path}` | STRING | same as TTL key | Cached ETag for conditional revalidation |
| `circuit:{remote}` | STRING | configurable | Circuit breaker — consecutive failure count |

Losing Redis = all TTLs expire = next request re-validates upstream. No data loss.

### S3 Layout (Content-Addressable)

```
artifacts-bucket/
├── blobs/sha256/{content_hash}     # immutable CAS blobs
├── indexes/{remote}/{path}          # mutable index files (helm, pypi, rpm, etc.)
├── indexes/{virtual}/{path}         # merged virtual indexes
└── local/{repo}/{path}              # user uploads (CAS-backed via blobs table)
```

---

## Terraform Remote Type (New in v2)

The `terraform` package type proxies the Terraform Provider Registry Protocol:

- **URL construction**: prepends `/v1/providers/` to request paths
- **Built-in mutable pattern**: `[^/]+/[^/]+/versions$` (version listings change over time)
- **Built-in immutable pattern**: `[^/]+/[^/]+/[^/]+/download/[^/]+/[^/]+$` (per-version download info is fixed)
- **Response rewriting**: download info JSON — rewrites `download_url`, `shasums_url`, `shasums_signature_url` to route through a companion `releases_remote` (e.g., `hashicorp-releases` generic remote)
- **Config**: requires `releases_remote` field pointing to the CDN remote that serves the actual binaries

Uses `github.com/hashicorp/terraform-registry-address` for address parsing and protocol-compliant URL generation.

---

## Go Module Proxy Remote Type (New)

The `goproxy` package type implements the GOPROXY protocol (Go module proxy):

| Endpoint | Mutability | Description |
|---|---|---|
| `{module}/@v/list` | Mutable | Plain text list of known versions |
| `{module}/@latest` | Mutable | JSON metadata for latest version |
| `{module}/@v/{version}.info` | Immutable | JSON version metadata (`Version`, `Time`) |
| `{module}/@v/{version}.mod` | Immutable | `go.mod` file for that version |
| `{module}/@v/{version}.zip` | Immutable | Source archive for that version |

- **No URL rewriting needed** — responses are self-contained (no embedded URLs)
- **Config**: `base_url` points to upstream proxy (e.g., `https://proxy.golang.org`)
- **Client usage**: set `GOPROXY=https://artifactapi.example.com/api/v1/remote/goproxy`
- Uses `github.com/goproxy/goproxy` for protocol handling

---

## Allowlist / Blocklist / Automatic Mutable Patterns

### Access Control (Per-Remote)

| Field | Default | Behavior |
|---|---|---|
| `blocklist` | `[]` (empty) | If a path matches any blocklist pattern → **403 Forbidden**. Checked first |
| `allowlist` | `[]` (empty) | If empty → **allow everything**. If non-empty → only matching paths are proxied; everything else → **403** |

Evaluation order: blocklist → allowlist → proxy. No allowlist + no blocklist = open proxy (default).

### Automatic Mutable Patterns (Per-Provider Built-ins)

Each provider declares built-in mutable patterns that are **always merged** with user-defined `mutable_patterns`. Users never need to configure these — the provider knows which paths change over time.

| Provider | Built-in Mutable Patterns | Rationale |
|---|---|---|
| **generic** | *(none)* | No convention for what's mutable |
| **docker** | `/manifests/(?!sha256:)[^/]+$`, `/tags/list$` | Tag manifests change; digest manifests don't |
| **helm** | `index\.yaml$` | Chart index changes when new charts are published |
| **pypi** | `simple/` | Package index pages change with new releases |
| **npm** | `^[^/]+$` (package metadata, not `.tgz`) | Package metadata changes; tarballs are immutable |
| **rpm** | `repomd\.xml$`, `repodata/.*`, `Packages\.gz$` | Repo metadata rebuilt on every publish |
| **alpine** | `APKINDEX\.tar\.gz$` | Package index rebuilt on every publish |
| **puppet** | `^v3/modules/`, `^v3/releases` | Module metadata changes with new releases |
| **terraform** | `[^/]+/[^/]+/versions$` | Provider version listings grow over time |
| **goproxy** | `@v/list$`, `@latest$` | Version list and latest pointer change |

These are returned by `Provider.BuiltinMutablePatterns()` and merged at classification time:
```
effective_mutable = provider.BuiltinMutablePatterns() ∪ remote.mutable_patterns
```

If a path matches `effective_mutable` → use `mutable_ttl`. If it matches `remote.immutable_patterns` → use `immutable_ttl`. Immutable patterns take precedence over mutable when both match.

---

## API Design

### v1 Proxy Endpoints (Backwards Compatible)

| Method | Path | Description |
|---|---|---|
| `GET` | `/api/v1/remote/{name}/{path}` | Proxy/cache artifact |
| `GET` | `/api/v1/virtual/{name}/{path}` | Virtual repo proxy |
| `GET/HEAD` | `/v2/{name}/{path}` | Docker Registry v2 |
| `GET` | `/v2/` | Docker v2 ping |
| `GET/PUT/HEAD/DELETE` | `/api/v1/local/{name}/{path}` | Local repo CRUD |

### v2 Management API (New)

```
GET    /api/v2/remotes                              → [{name, package_type, base_url, description, stats}]
GET    /api/v2/remotes/{name}                       → {full config + stats + health}
POST   /api/v2/remotes                              → create remote (Terraform provider)
PUT    /api/v2/remotes/{name}                        → update remote (Terraform provider)
DELETE /api/v2/remotes/{name}                        → delete remote — cascades artifacts, GC cleans S3

GET    /api/v2/virtuals                             → [{name, package_type, members, stats}]
GET    /api/v2/virtuals/{name}                      → {full config + member details}
POST   /api/v2/virtuals                             → create virtual
PUT    /api/v2/virtuals/{name}                       → update virtual
DELETE /api/v2/virtuals/{name}                       → delete virtual

GET    /api/v2/remotes/{name}/objects               → paginated objects
         ?q=pattern&sort=size|accessed|age&page=1&per_page=50
DELETE /api/v2/remotes/{name}/objects/{path}         → evict specific cached object
DELETE /api/v2/remotes/{name}/cache                 → flush cache
         ?type=all|indexes|blobs

GET    /api/v2/stats                                → overview stats
GET    /api/v2/stats/top-remotes                    → top remotes by size/requests/hit-rate

GET    /api/v2/health                               → {status, postgres, redis, s3, uptime}
GET    /metrics                                     → Prometheus format
GET    /api/v2/events                               → SSE stream
```

---

## Proxy Engine

### Request Flow

```
Client Request
    │
    ▼
Classify (immutable/mutable/denied)
    │
    ├── blocklist match → 403
    ├── allowlist non-empty + no match → 403
    │
    ▼
Check Redis TTL key
    │
    ├── exists (fresh) → serve from S3, log access
    │
    ├── missing (expired or uncached)
    │   │
    │   ▼
    │   Acquire fetch lock (Redis SETNX, 30s TTL)
    │   │
    │   ├── lock acquired
    │   │   ├── mutable + check_mutable + have ETag → HEAD upstream
    │   │   │   ├── 304 → refresh TTL, serve from S3
    │   │   │   └── changed → full fetch
    │   │   └── full fetch from upstream
    │   │       → provider.RewriteResponse() if needed
    │   │       → CAS store (hash → check blobs → upload if new)
    │   │       → upsert artifact in Postgres
    │   │       → set Redis TTL + release lock
    │   │       → on upstream error + stale_on_error → refresh TTL, serve stale
    │   │
    │   └── lock not acquired → poll S3 briefly, serve if another pod fetched it
    │
    ▼
Stream response from S3, log access
```

### Circuit Breaker

Per-remote, tracked in Redis. Closed → Open (after N failures) → Half-open (after cooldown). Exposed via `GET /api/v2/remotes/{name}` health field.

### Content-Addressable Storage

1. Stream upstream → temp file, compute SHA256 inline
2. Check `blobs` table for hash
3. Exists → skip S3 upload, upsert `artifacts` row only
4. New → upload to `blobs/sha256/{hash}`, insert both rows

### Garbage Collection

Background goroutine (configurable interval, default 1h):
1. Orphaned blobs: delete S3 objects whose `content_hash` has no referencing `artifacts` or `local_files` rows
2. Cold artifacts: optional per-remote, delete artifacts not accessed in N days
3. Remote deletion: `ON DELETE CASCADE` handles Postgres; GC sweeps orphaned blobs

---

## Package Providers

### Provider Interface

```go
type Provider interface {
    Type() models.PackageType
    BuiltinMutablePatterns() []*regexp.Regexp
    BuiltinImmutablePatterns() []*regexp.Regexp
    ContentType(path string) string
    UpstreamURL(remote models.Remote, path string) string
    RewriteResponse(body []byte, remote models.Remote, proxyBaseURL string) ([]byte, error)
    AuthHeaders(ctx context.Context, remote models.Remote) (http.Header, error)
}

type IndexMerger interface {
    MergeIndexes(members []MemberIndex, proxyBaseURL string) ([]byte, error)
}
```

### Provider Registry

```go
var registry = map[models.PackageType]Provider{
    models.PackageGeneric:   &generic.Provider{},
    models.PackageDocker:    &docker.Provider{},
    models.PackageHelm:      &helm.Provider{},
    models.PackagePyPI:      &pypi.Provider{},
    models.PackageNPM:       &npm.Provider{},
    models.PackageRPM:       &rpm.Provider{},
    models.PackageAlpine:    &alpine.Provider{},
    models.PackagePuppet:    &puppet.Provider{},
    models.PackageTerraform: &terraform.Provider{},
    models.PackageGoProxy:   &goproxy.Provider{},
}

func Get(t models.PackageType) (Provider, error) { ... }
```

Each provider lives in its own subpackage under `internal/provider/` with its own `_test.go`.

---

## Testing Strategy

### Unit Tests

Every package gets `_test.go` files alongside the source. Run with `go test ./...`.

| Package | What's Tested |
|---|---|
| `internal/provider/docker/` | Auth token parsing/caching, manifest classification, tag banning, URL construction, blob key generation |
| `internal/provider/helm/` | `index.yaml` parsing (using `helm.sh/helm/v3/pkg/repo`), URL rewriting, index merging |
| `internal/provider/pypi/` | Simple index HTML parsing, URL rewriting, index merging |
| `internal/provider/npm/` | Metadata JSON rewriting (`dist.tarball` URLs) |
| `internal/provider/terraform/` | Registry URL construction, download info JSON rewriting, `releases_remote` URL extraction |
| `internal/provider/rpm/` | Mutable pattern matching (repodata) |
| `internal/provider/alpine/` | Mutable pattern matching (APKINDEX) |
| `internal/provider/puppet/` | `file_uri` JSON rewriting |
| `internal/proxy/` | Classifier (immutable vs mutable vs denied), circuit breaker state transitions, revalidator logic |
| `internal/storage/` | CAS key generation, dedup detection, S3 operation mocking |
| `internal/cache/` | Redis TTL set/check, fetch lock acquire/release/contention |
| `internal/gc/` | Orphan detection queries, cold artifact selection |
| `pkg/models/` | Model validation, PackageType enum |
| `pkg/client/` | API client request/response serialization |

### End-to-End Tests

Located in `e2e/`. Use `testcontainers-go` to spin up real Postgres, Redis, and MinIO containers. The test binary starts the actual `artifactapi` server against these backends.

```go
// e2e/e2e_test.go
func TestMain(m *testing.M) {
    // Start postgres, redis, minio via testcontainers-go
    // Run migrations
    // Start artifactapi server on random port
    // Run tests
    // Tear down
}
```

| Test File | What's Tested |
|---|---|
| `e2e/proxy_test.go` | Proxy a real GitHub release through generic remote, verify S3 storage, verify Redis TTL, verify Postgres artifact row, verify cache hit on second request |
| `e2e/docker_test.go` | Pull a real image manifest + blob through Docker v2 proxy, verify blob deduplication, tag banning |
| `e2e/management_test.go` | Full CRUD lifecycle: create remote via v2 API, proxy through it, list objects, evict object, flush cache, delete remote |
| `e2e/virtual_test.go` | Create two helm remotes + virtual, fetch merged index, verify priority ordering |
| `e2e/terraform_test.go` | Proxy terraform provider version listing + download info, verify URL rewriting to releases_remote |
| `e2e/goproxy_test.go` | Proxy Go module `@v/list`, `.info`, `.mod`, `.zip` through GOPROXY remote, verify mutable vs immutable classification |
| `e2e/gc_test.go` | Create artifact, delete remote, trigger GC, verify S3 blob cleaned up |

### Code Quality

- `gofmt` / `goimports` — enforced in CI, run on save
- `golangci-lint` — comprehensive linter suite (staticcheck, errcheck, govet, etc.)
- `go vet ./...` — run in CI
- Makefile targets: `make test`, `make lint`, `make e2e`, `make fmt`

---

## Terraform Provider (Separate Repo)

**Repo**: `terraform-provider-artifactapi`
**Uses**: `pkg/client/` and `pkg/models/` from the main module

```hcl
provider "artifactapi" {
  endpoint = "https://artifactapi.example.com"
}

resource "artifactapi_remote" "terraform_registry" {
  name            = "terraform-registry"
  package_type    = "terraform"
  base_url        = "https://registry.terraform.io"
  description     = "Terraform provider registry"
  releases_remote = artifactapi_remote.hashicorp_releases.name

  immutable_patterns = [
    "[^/]+/[^/]+/[^/]+/download/[^/]+/[^/]+$",
  ]

  cache {
    immutable_ttl = 0
    mutable_ttl   = 300
  }
}

resource "artifactapi_remote" "hashicorp_releases" {
  name         = "hashicorp-releases"
  package_type = "generic"
  base_url     = "https://releases.hashicorp.com"

  immutable_patterns = [
    ".*\\.zip$",
    ".*SHA256SUMS(\\.sig)?$",
  ]

  cache {
    immutable_ttl = 0
    mutable_ttl   = 0
  }
}

resource "artifactapi_virtual" "helm" {
  name         = "helm"
  package_type = "helm"
  description  = "All helm repos merged"
  members      = [
    artifactapi_remote.jetstack.name,
    artifactapi_remote.hashicorp_helm.name,
  ]
}
```

---

## Web UI (React + Vite — Separate Container)

### Deployment

Separate `Dockerfile.ui`: multi-stage build (node → nginx). Served as its own container/pod. nginx proxies `/api/*` to the Go backend.

### Pages

| Route | Content |
|---|---|
| `/` | Dashboard: total objects, storage used, dedup savings, bandwidth saved, top remotes chart, live SSE event feed, health indicators |
| `/remotes` | Remote table: name, type, description, object count, size, hit rate, health. Filter by type, sort any column |
| `/remotes/:name` | Config (read-only, "Managed by Terraform" badge), stats, object browser with search/sort/evict, flush actions |
| `/virtuals` | Virtual table: name, type, members, merged object count |
| `/virtuals/:name` | Member list with individual stats |

All config is read-only — managed by Terraform.

---

## TUI (Bubble Tea — Subcommand)

`artifactapi tui --endpoint http://localhost:8000` or via `ARTIFACTAPI_ENDPOINT` env.

Uses `pkg/client/` for all API calls (same client as Terraform provider).

| View | Key bindings |
|---|---|
| Dashboard | summary stats, top remotes |
| Remotes list | `j`/`k` navigate, `/` filter, `Enter` detail |
| Remote detail | config + stats, `Enter` → object browser |
| Object browser | `/` search, `d` evict, `f` flush |
| Virtuals | `j`/`k`, `Enter` detail |

---

## Improvements Over v2

| Area | v2 (Python) | v3 (Go) |
|---|---|---|
| S3 paths | Hashed, opaque | Content-addressed CAS |
| Config | YAML files, mtime reload | Terraform via API |
| Package types | 8 types | 10 types (+ terraform, goproxy) |
| Virtual repos | Helm only | Helm + PyPI, extensible |
| Deduplication | Docker blobs only | All types via CAS |
| Revalidation | Opt-in flag | Default for all mutable |
| Access logging | None | Per-artifact in Postgres |
| GC | None | Background goroutine |
| Upstream health | Per-request | Circuit breaker |
| S3 backends | MinIO only | MinIO, Ceph, AWS (minio-go) |
| UI | None | Web dashboard + TUI |
| Binary | Python + venv | Static Go binary |
| Frontend | N/A | Separate container (React) |
| Testing | Mocked unit tests | Unit + e2e with real backends |

---

## Implementation Phases

### Phase 1: Core Engine + Models
- Go module, Makefile (`make build test lint fmt e2e`), Dockerfile, docker-compose
- `pkg/models/` — all domain types
- PostgreSQL schema + migrations
- S3 storage layer with CAS (`minio-go/v7`)
- Redis cache layer (TTL, locks)
- Proxy engine: fetch-or-cache, classifier, revalidator
- Generic + Docker providers (most complexity: OCI auth, CAS, tag banning)
- Health + metrics endpoints
- Unit tests for all packages
- **Milestone**: proxy Docker + generic, cache in S3, track in Postgres

### Phase 2: All Providers
- Helm (using `helm.sh/helm/v3/pkg/repo`)
- PyPI (stdlib `x/net/html`)
- npm (stdlib `encoding/json`)
- RPM (using `rs3.io/go/rpm/repomd`)
- Alpine (using `gitlab.alpinelinux.org/alpine/go`)
- Puppet Forge (stdlib `encoding/json`)
- Terraform (using `hashicorp/terraform-registry-address`)
- Go Modules / GOPROXY (using `github.com/goproxy/goproxy`)
- Unit tests per provider
- **Milestone**: feature parity with v2 + goproxy

### Phase 3: Management API + Virtual Repos + GC
- `pkg/client/` — shared Go API client
- v2 CRUD endpoints
- Virtual repo engine: `IndexMerger` for Helm + PyPI
- Circuit breaker
- Access logging middleware
- GC goroutine
- **Milestone**: full API, virtuals, GC

### Phase 4: End-to-End Tests
- `e2e/` test suite with `testcontainers-go`
- Proxy, Docker, management, virtual, terraform, GC tests
- CI pipeline: `make e2e`
- **Milestone**: comprehensive e2e coverage

### Phase 5: Terraform Provider
- Separate repo: `terraform-provider-artifactapi`
- Imports `pkg/client/` and `pkg/models/`
- `artifactapi_remote` + `artifactapi_virtual` resources + data sources
- Import support
- **Milestone**: manage all config via Terraform

### Phase 6: Web UI
- React + Vite in `ui/`
- `Dockerfile.ui` (multi-stage → nginx)
- Dashboard, remotes, objects, virtuals pages
- SSE event feed
- **Milestone**: full web UI in separate container

### Phase 7: TUI
- Bubble Tea in `internal/tui/`
- Uses `pkg/client/`
- Dashboard, remotes, objects, virtuals views
- **Milestone**: TUI feature parity with web UI

### Phase 8: Migration + Cutover
- Migration tool: v2 YAML → Terraform HCL + `terraform import` commands
- S3 rehash script: `{remote}/{hash16}/{file}` → `blobs/sha256/{content_hash}`
- Parallel run, response comparison
- Cutover

---

## Makefile Targets

```makefile
.PHONY: build test lint fmt e2e docker docker-ui

build:                  ## Build Go binary
	go build -o bin/artifactapi ./cmd/artifactapi

test:                   ## Run unit tests
	go test ./...

lint:                   ## Run golangci-lint + go vet
	golangci-lint run ./...
	go vet ./...

fmt:                    ## Format code (gofmt + goimports)
	gofmt -w .
	goimports -w .

e2e:                    ## Run end-to-end tests (requires Docker)
	go test -tags=e2e -count=1 -timeout=5m ./e2e/...

docker:                 ## Build API server Docker image
	docker build -t artifactapi .

docker-ui:              ## Build frontend Docker image
	docker build -t artifactapi-ui -f ui/Dockerfile.ui ui/

compose:                ## Start full stack (API + UI + Postgres + Redis + MinIO)
	docker compose up -d
```
