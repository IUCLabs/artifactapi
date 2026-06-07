# ArtifactAPI

Caching proxy for package repositories. Single Go binary, 10 package types, content-addressable storage, managed by Terraform.

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

Providers classify paths automatically. Users only configure what to proxy and TTLs.

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

## Environment Variables

| Variable | Default | Description |
|---|---|---|
| `LISTEN_ADDR` | `:8000` | Server listen address |
| `DBHOST` | `localhost` | PostgreSQL host |
| `DBPORT` | `5432` | PostgreSQL port |
| `DBUSER` | `artifacts` | PostgreSQL user |
| `DBPASS` | | PostgreSQL password |
| `DBNAME` | `artifacts` | PostgreSQL database |
| `REDIS_URL` | `redis://localhost:6379` | Redis URL |
| `MINIO_ENDPOINT` | `localhost:9000` | S3 endpoint |
| `MINIO_ACCESS_KEY` | | S3 access key |
| `MINIO_SECRET_KEY` | | S3 secret key |
| `MINIO_BUCKET` | `artifacts` | S3 bucket |
| `MINIO_SECURE` | `false` | Use HTTPS for S3 |
| `MINIO_REGION` | | S3 region (AWS) |

## Development

```bash
make build       # Build binary
make test        # Unit tests
make e2e         # E2E tests (needs Docker)
make lint        # golangci-lint + go vet
make fmt         # gofmt + goimports
```

### TUI

```bash
./bin/artifactapi tui --endpoint http://localhost:8000
```
