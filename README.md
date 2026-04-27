# Artifact Storage System

A generic FastAPI-based artifact caching system that downloads and stores files from remote sources (GitHub, Gitea, HashiCorp, etc.) in S3-compatible storage with configuration-based access control.

## Features

- **Generic Remote Support**: Works with any HTTP-based file server (GitHub, Gitea, HashiCorp, custom servers)
- **Configuration-Based**: YAML configuration for remotes, patterns, and access control
- **Direct URL API**: Access cached files via clean URLs like `/api/v1/remote/github/owner/repo/path/file.tar.gz`
- **Immutable/Mutable Pattern Model**: Per-remote regex patterns distinguish forever-cached artifacts from TTL-expiring metadata
- **Smart Caching**: Automatic download and cache on first access, serve from cache afterward
- **Conditional Revalidation**: Optional `check_mutable_updates` flag — sends `If-None-Match`/`If-Modified-Since` on expiry; skips re-download on 304
- **Stale-on-Upstream-Error**: Expired mutable files are kept and their TTL refreshed when the backend cannot be reached, so cached data remains available during upstream outages
- **S3 Storage**: MinIO/S3 backend with predictable paths
- **Docker Registry Proxy**: Full Docker Registry HTTP API v2 for transparent container image caching
- **Content-Type Detection**: Automatic MIME type detection for downloads

## Architecture

The system acts as a caching proxy that:
1. Receives requests via the `/api/{remote}/{path}` endpoint
2. Checks if the file is already cached
3. If not cached, downloads from the configured remote and caches it
4. Serves the file with appropriate headers and content types
5. Enforces access control via configurable regex patterns

## Quick Start

1. Start MinIO container:
```bash
docker-compose up -d
```

2. Create virtual environment and install dependencies:
```bash
uv venv
source .venv/bin/activate
uv pip install -r requirements.txt
```

3. Start the API:
```bash
python main.py
```

4. Access artifacts directly via URL:
```bash
# This will download and cache the file on first access
xh GET localhost:8000/api/github/gruntwork-io/terragrunt/releases/download/v0.96.1/terragrunt_linux_amd64.tar.gz

# Subsequent requests serve from cache (see X-Artifact-Source: cache header)
curl -I localhost:8000/api/github/gruntwork-io/terragrunt/releases/download/v0.96.1/terragrunt_linux_amd64.tar.gz
```

## API Endpoints

### Direct Access
- `GET /api/{remote}/{path}` - Direct access to artifacts with auto-caching

### Management
- `GET /` - API info and available remotes
- `GET /health` - Health check
- `GET /config` - View current configuration
- `POST /cache-artifact` - Batch cache artifacts matching pattern
- `GET /artifacts/{remote}` - List cached artifacts

## Configuration

The system uses `remotes.yaml` to define remote repositories and access patterns. All other configuration is provided via environment variables.

### remotes.yaml Structure

```yaml
remotes:
  remote-name:
    base_url: "https://example.com"       # Base URL for the remote
    type: "remote"                        # "remote" or "local"
    package: "generic"                    # "generic", "alpine", "rpm", or "docker"
    description: "Human readable description"
    immutable_patterns:                   # Files cached forever (release binaries, versioned tags)
      - "pattern1"
      - "pattern2"
    mutable_patterns:                     # Files that expire after mutable_ttl (optional)
      - "pattern3"
    check_mutable_updates: false          # Enable conditional HEAD before re-fetching (optional)
    cache:
      immutable_ttl: 0                    # TTL for immutable files (0 = indefinitely)
      mutable_ttl: 3600                   # TTL in seconds for mutable files
```

### Remote Types

#### Generic Remotes
For general file hosting (GitHub releases, custom servers):

```yaml
remotes:
  github:
    base_url: "https://github.com"
    type: "remote"
    package: "generic"
    description: "GitHub releases and files"
    immutable_patterns:
      - "gruntwork-io/terragrunt/.*terragrunt_linux_amd64.*"
      - "lxc/incus/.*\\.tar\\.gz$"
      - "prometheus/node_exporter/.*/node_exporter-.*\\.linux-amd64\\.tar\\.gz$"
    cache:
      immutable_ttl: 0      # Cache files indefinitely

  github-archive:
    base_url: "https://github.com"
    type: "remote"
    package: "generic"
    description: "GitHub repository archive tarballs"
    immutable_patterns:
      - ".*/archive/refs/tags/.*\\.tar\\.gz$"   # tag archives never change
    mutable_patterns:
      - ".*/archive/refs/heads/main\\.tar\\.gz$"  # branch archives can change
    check_mutable_updates: true   # send If-None-Match on expiry; skip re-download on 304
    cache:
      immutable_ttl: 0
      mutable_ttl: 86400          # re-check branch archives after 1 day
```

#### Package Repository Remotes
For Linux package repositories:

```yaml
remotes:
  alpine:
    base_url: "https://dl-cdn.alpinelinux.org"
    type: "remote"
    package: "alpine"
    description: "Alpine Linux APK package repository"
    immutable_patterns:
      - ".*/x86_64/.*\\.apk$"     # packages are immutable by content-hash
    # APKINDEX.tar.gz is a package-type default mutable file — no mutable_patterns needed
    cache:
      immutable_ttl: 0
      mutable_ttl: 7200            # re-fetch APKINDEX.tar.gz after 2 hours

  almalinux:
    base_url: "https://mirror.example.com/almalinux"
    type: "remote"
    package: "rpm"
    description: "AlmaLinux RPM package repository"
    immutable_patterns:
      - ".*/x86_64/.*\\.rpm$"
      - ".*/noarch/.*\\.rpm$"
    # repomd.xml and repodata/* are package-type defaults
    cache:
      immutable_ttl: 0
      mutable_ttl: 7200
```

#### Local Repositories
For storing custom artifacts:

```yaml
remotes:
  local-generic:
    type: "local"
    package: "generic"
    description: "Local generic file repository"
    cache:
      immutable_ttl: 0
      mutable_ttl: 0
```

### Immutable Patterns

`immutable_patterns` are regular expressions that control which files can be accessed. Patterns use Python `re.search`, so they match anywhere in the path unless anchored with `^` or `$`. Only files matching at least one pattern are served; all others return HTTP 403.

Matched files are cached with `immutable_ttl` (default 0 = forever). Use these for versioned release artifacts that never change once published.

```yaml
immutable_patterns:
  - "^gruntwork-io/terragrunt/releases/download/.*/terragrunt_linux_amd64$"
  - "gruntwork-io/terragrunt/.*terragrunt_linux_amd64.*"
  - ".*\\.tar\\.gz$"
  - ".*/x86_64/.*\\.rpm$"
  - ".*/noarch/.*\\.rpm$"
  - ".*/repodata/.*$"
```

**Security note**: Omitting `immutable_patterns` entirely allows all files from that remote.

### Mutable Patterns

`mutable_patterns` identify files that change over time (index files, branch archives, metadata). Mutable files:
- **Always served** regardless of `immutable_patterns`
- **Cached with `mutable_ttl`** and re-fetched from upstream when the TTL expires
- **Kept stale** when the upstream backend is unreachable — TTL is refreshed automatically so the cached copy remains available until the backend recovers (see below)

Built-in defaults per package type (no configuration needed):

| Package type | Built-in mutable patterns |
|---|---|
| `alpine` | `APKINDEX\.tar\.gz$` |
| `rpm` | `repomd\.xml$`, `repodata/` metadata (xml, sqlite, yaml, asc, txt variants), `Packages\.gz$` |
| `docker` | Tag manifests (non-digest refs), `/tags/list` |
| `generic` | *(none)* |

Use `mutable_patterns` to add extra patterns on top of the defaults. Duplicates are ignored automatically.

```yaml
remotes:
  helm-charts:
    base_url: "https://charts.example.com"
    type: "remote"
    package: "generic"
    immutable_patterns:
      - ".*\\.tgz$"
    mutable_patterns:
      - "index\\.yaml$"        # Helm repo index
    cache:
      immutable_ttl: 0
      mutable_ttl: 600         # re-check the index every 10 minutes

  apt-mirror:
    base_url: "https://apt.example.com"
    type: "remote"
    package: "generic"
    immutable_patterns:
      - ".*\\.deb$"
    mutable_patterns:
      - "InRelease$"
      - "Release$"
      - "Packages\\.gz$"
      - "Packages\\.xz$"
    cache:
      immutable_ttl: 0
      mutable_ttl: 3600
```

### Conditional Revalidation (`check_mutable_updates`)

By default, when a mutable file's TTL expires the cached copy is evicted and the full file is re-downloaded on the next request. Setting `check_mutable_updates: true` on a remote enables a cheaper conditional check first:

1. On TTL expiry, a `HEAD` request is sent to the upstream with `If-None-Match` / `If-Modified-Since` headers (populated from the original download).
2. If the upstream replies **304 Not Modified**, the TTL is refreshed in place — no re-download, no S3 traffic.
3. If the upstream replies **200**, the cached copy is evicted and re-downloaded normally.

This only applies to user-defined `mutable_patterns`. Package-type built-in patterns (APKINDEX, repomd.xml, Docker manifests) are always re-fetched unconditionally.

```yaml
remotes:
  github-archive:
    base_url: "https://github.com"
    type: "remote"
    package: "generic"
    immutable_patterns:
      - ".*/archive/refs/tags/.*\\.tar\\.gz$"
    mutable_patterns:
      - ".*/archive/refs/heads/main\\.tar\\.gz$"
    check_mutable_updates: true
    cache:
      immutable_ttl: 0
      mutable_ttl: 86400
```

### Stale-on-Upstream-Error

When a mutable file's TTL expires and the upstream backend **cannot be reached** (connection refused, DNS failure, timeout), the cached copy is **kept and its TTL refreshed** rather than evicted. This means:

- RPM repodata, Alpine indexes, branch archives, and other mutable files remain available during upstream outages.
- Clients continue to receive the last-known-good copy without errors.
- Once the backend recovers and the refreshed TTL next expires, normal eviction resumes.

This behaviour is automatic and requires no configuration. Only network-level failures trigger it — HTTP error responses (404, 503, etc.) are treated as the backend being reachable and proceed with normal expiry.

### Cache Configuration

```yaml
cache:
  immutable_ttl: 0       # Immutable files (0 = cache indefinitely, rarely changed)
  mutable_ttl: 3600      # Mutable files — TTL in seconds before re-fetch is attempted
```

### Environment Variables

All runtime configuration comes from environment variables:

**Database Configuration:**
- `DBHOST` - PostgreSQL host
- `DBPORT` - PostgreSQL port
- `DBUSER` - PostgreSQL username
- `DBPASS` - PostgreSQL password
- `DBNAME` - PostgreSQL database name

**Redis Configuration:**
- `REDIS_URL` - Redis connection URL (e.g., `redis://localhost:6379`)

**S3/MinIO Configuration:**
- `MINIO_ENDPOINT` - MinIO/S3 endpoint
- `MINIO_ACCESS_KEY` - S3 access key
- `MINIO_SECRET_KEY` - S3 secret key
- `MINIO_BUCKET` - S3 bucket name
- `MINIO_SECURE` - Use HTTPS (`true`/`false`)

## Usage Examples

### Direct File Access
```bash
# Access GitHub releases
curl localhost:8000/api/github/gruntwork-io/terragrunt/releases/download/v0.96.1/terragrunt_linux_amd64.tar.gz

# Access HashiCorp releases (when configured)
curl localhost:8000/api/hashicorp/terraform/1.6.0/terraform_1.6.0_linux_amd64.zip

# Access custom remotes
curl localhost:8000/api/custom/path/to/file.tar.gz
```

### Response Headers
- `X-Artifact-Source: cache|remote` - Indicates if served from cache or freshly downloaded
- `Content-Type` - Automatically detected (application/gzip, application/zip, etc.)
- `Content-Disposition` - Download filename
- `Content-Length` - File size

### Pattern Enforcement
Access is controlled by regex patterns in the configuration. Requests for files not matching any pattern return HTTP 403.

## Storage Path Format

Files are stored with keys like:
- `{remote_name}/{path_hash}/{filename}` for direct API access
- `{hostname}/{url_hash}/{filename}` for legacy batch operations

Example: `github/a1b2c3d4e5f6g7h8/terragrunt_linux_amd64.tar.gz`

## Kubernetes Deployment

Deploy the artifact storage system to Kubernetes using the following manifests:

### 1. Namespace
```yaml
apiVersion: v1
kind: Namespace
metadata:
  name: artifact-storage
```

### 2. ConfigMap for remotes.yaml
```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: artifactapi-config
  namespace: artifact-storage
data:
  remotes.yaml: |
    remotes:
      github:
        base_url: "https://github.com"
        type: "remote"
        package: "generic"
        description: "GitHub releases and files"
        immutable_patterns:
          - "gruntwork-io/terragrunt/.*terragrunt_linux_amd64.*"
          - "lxc/incus/.*\\.tar\\.gz$"
          - "prometheus/node_exporter/.*/node_exporter-.*\\.linux-amd64\\.tar\\.gz$"
        cache:
          immutable_ttl: 0
          mutable_ttl: 0

      hashicorp-releases:
        base_url: "https://releases.hashicorp.com"
        type: "remote"
        package: "generic"
        description: "HashiCorp product releases"
        immutable_patterns:
          - "terraform/.*terraform_.*_linux_amd64\\.zip$"
          - "vault/.*vault_.*_linux_amd64\\.zip$"
          - "consul/.*/consul_.*_linux_amd64\\.zip$"
        cache:
          immutable_ttl: 0
          mutable_ttl: 0
```

### 3. Secret for Environment Variables
```yaml
apiVersion: v1
kind: Secret
metadata:
  name: artifactapi-secret
  namespace: artifact-storage
type: Opaque
stringData:
  DBHOST: "postgres-service"
  DBPORT: "5432"
  DBUSER: "artifacts"
  DBPASS: "artifacts123"
  DBNAME: "artifacts"
  REDIS_URL: "redis://redis-service:6379"
  MINIO_ENDPOINT: "minio-service:9000"
  MINIO_ACCESS_KEY: "minioadmin"
  MINIO_SECRET_KEY: "minioadmin"
  MINIO_BUCKET: "artifacts"
  MINIO_SECURE: "false"
```

### 4. PostgreSQL Deployment
```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: postgres
  namespace: artifact-storage
spec:
  replicas: 1
  selector:
    matchLabels:
      app: postgres
  template:
    metadata:
      labels:
        app: postgres
    spec:
      containers:
      - name: postgres
        image: postgres:15-alpine
        env:
        - name: POSTGRES_DB
          value: artifacts
        - name: POSTGRES_USER
          value: artifacts
        - name: POSTGRES_PASSWORD
          value: artifacts123
        ports:
        - containerPort: 5432
        volumeMounts:
        - name: postgres-storage
          mountPath: /var/lib/postgresql/data
        livenessProbe:
          exec:
            command: ["pg_isready", "-U", "artifacts", "-d", "artifacts"]
          initialDelaySeconds: 30
          periodSeconds: 30
      volumes:
      - name: postgres-storage
        persistentVolumeClaim:
          claimName: postgres-pvc
---
apiVersion: v1
kind: Service
metadata:
  name: postgres-service
  namespace: artifact-storage
spec:
  selector:
    app: postgres
  ports:
  - port: 5432
    targetPort: 5432
---
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: postgres-pvc
  namespace: artifact-storage
spec:
  accessModes:
    - ReadWriteOnce
  resources:
    requests:
      storage: 10Gi
```

### 5. Redis Deployment
```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: redis
  namespace: artifact-storage
spec:
  replicas: 1
  selector:
    matchLabels:
      app: redis
  template:
    metadata:
      labels:
        app: redis
    spec:
      containers:
      - name: redis
        image: redis:7-alpine
        command: ["redis-server", "--save", "20", "1"]
        ports:
        - containerPort: 6379
        volumeMounts:
        - name: redis-storage
          mountPath: /data
        livenessProbe:
          exec:
            command: ["redis-cli", "ping"]
          initialDelaySeconds: 30
          periodSeconds: 30
      volumes:
      - name: redis-storage
        persistentVolumeClaim:
          claimName: redis-pvc
---
apiVersion: v1
kind: Service
metadata:
  name: redis-service
  namespace: artifact-storage
spec:
  selector:
    app: redis
  ports:
  - port: 6379
    targetPort: 6379
---
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: redis-pvc
  namespace: artifact-storage
spec:
  accessModes:
    - ReadWriteOnce
  resources:
    requests:
      storage: 5Gi
```

### 6. MinIO Deployment
```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: minio
  namespace: artifact-storage
spec:
  replicas: 1
  selector:
    matchLabels:
      app: minio
  template:
    metadata:
      labels:
        app: minio
    spec:
      containers:
      - name: minio
        image: minio/minio:latest
        command: ["minio", "server", "/data", "--console-address", ":9001"]
        env:
        - name: MINIO_ROOT_USER
          value: minioadmin
        - name: MINIO_ROOT_PASSWORD
          value: minioadmin
        ports:
        - containerPort: 9000
        - containerPort: 9001
        volumeMounts:
        - name: minio-storage
          mountPath: /data
        livenessProbe:
          httpGet:
            path: /minio/health/live
            port: 9000
          initialDelaySeconds: 30
          periodSeconds: 30
      volumes:
      - name: minio-storage
        persistentVolumeClaim:
          claimName: minio-pvc
---
apiVersion: v1
kind: Service
metadata:
  name: minio-service
  namespace: artifact-storage
spec:
  selector:
    app: minio
  ports:
  - name: api
    port: 9000
    targetPort: 9000
  - name: console
    port: 9001
    targetPort: 9001
---
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: minio-pvc
  namespace: artifact-storage
spec:
  accessModes:
    - ReadWriteOnce
  resources:
    requests:
      storage: 50Gi
```

### 7. Artifact API Deployment
```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: artifactapi
  namespace: artifact-storage
spec:
  replicas: 2
  selector:
    matchLabels:
      app: artifactapi
  template:
    metadata:
      labels:
        app: artifactapi
    spec:
      containers:
      - name: artifactapi
        image: artifactapi:latest
        ports:
        - containerPort: 8000
        envFrom:
        - secretRef:
            name: artifactapi-secret
        volumeMounts:
        - name: config-volume
          mountPath: /app/remotes.yaml
          subPath: remotes.yaml
        livenessProbe:
          httpGet:
            path: /health
            port: 8000
          initialDelaySeconds: 30
          periodSeconds: 30
        readinessProbe:
          httpGet:
            path: /health
            port: 8000
          initialDelaySeconds: 10
          periodSeconds: 5
        resources:
          requests:
            memory: "256Mi"
            cpu: "250m"
          limits:
            memory: "512Mi"
            cpu: "500m"
      volumes:
      - name: config-volume
        configMap:
          name: artifactapi-config
---
apiVersion: v1
kind: Service
metadata:
  name: artifactapi-service
  namespace: artifact-storage
spec:
  selector:
    app: artifactapi
  ports:
  - port: 8000
    targetPort: 8000
  type: ClusterIP
```

### 8. Ingress (Optional)
```yaml
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata:
  name: artifactapi-ingress
  namespace: artifact-storage
  annotations:
    nginx.ingress.kubernetes.io/rewrite-target: /
    nginx.ingress.kubernetes.io/proxy-body-size: "10g"
    nginx.ingress.kubernetes.io/proxy-read-timeout: "600"
spec:
  rules:
  - host: artifacts.example.com
    http:
      paths:
      - path: /
        pathType: Prefix
        backend:
          service:
            name: artifactapi-service
            port:
              number: 8000
```

### Deployment Commands
```bash
# Create namespace
kubectl apply -f namespace.yaml

# Deploy PostgreSQL, Redis, and MinIO
kubectl apply -f postgres.yaml
kubectl apply -f redis.yaml
kubectl apply -f minio.yaml

# Wait for databases to be ready
kubectl wait --for=condition=ready pod -l app=postgres -n artifact-storage --timeout=300s
kubectl wait --for=condition=ready pod -l app=redis -n artifact-storage --timeout=300s
kubectl wait --for=condition=ready pod -l app=minio -n artifact-storage --timeout=300s

# Deploy configuration and application
kubectl apply -f configmap.yaml
kubectl apply -f secret.yaml
kubectl apply -f artifactapi.yaml

# Optional: Deploy ingress
kubectl apply -f ingress.yaml

# Check deployment status
kubectl get pods -n artifact-storage
kubectl logs -f deployment/artifactapi -n artifact-storage
```

### Access the API
```bash
# Port-forward to access locally
kubectl port-forward service/artifactapi-service 8000:8000 -n artifact-storage

# Test the API
curl http://localhost:8000/health
curl http://localhost:8000/

# Access artifacts
curl "http://localhost:8000/api/github/gruntwork-io/terragrunt/releases/download/v0.96.1/terragrunt_linux_amd64"
```

### Notes for Production
- Use proper secrets management (e.g., Vault, Sealed Secrets)
- Configure resource limits and requests appropriately
- Set up monitoring and alerting
- Use external managed databases for production workloads
- Configure backup strategies for persistent volumes
- Set up proper TLS certificates for ingress
- Consider using StatefulSets for databases with persistent storage

## Docker Image Rewriting with RKE2

RKE2 can route container image pulls through registry mirrors using `/etc/rancher/rke2/registries.yaml`. The artifact API implements the Docker Registry HTTP API v2 at `/v2/`, so it acts as a transparent caching mirror for any upstream registry.

### How it works

1. A pod requests `docker.io/library/nginx:latest`
2. RKE2 intercepts the pull and rewrites the image path using the `rewrite` rules
3. The rewritten request hits the artifact API (`/v2/dockerhub/library/nginx/manifests/latest`)
4. On first access the API fetches the manifest and layers from Docker Hub and caches them in S3
5. Subsequent pulls are served directly from cache, with no upstream traffic

### registries.yaml

Place this file on every RKE2 node at `/etc/rancher/rke2/registries.yaml`. The `rewrite` field maps the original image path (as the upstream registry sees it) to the path the artifact API expects under `/v2/{remote_name}/...`.

#### Docker Hub

Docker Hub resolves unqualified image names like `nginx` as `library/nginx`. The rewrite prepends the remote name so the request lands on the correct remote.

```yaml
# /etc/rancher/rke2/registries.yaml
mirrors:
  docker.io:
    endpoint:
      - "https://artifacts.example.com"
    rewrite:
      "^(.*)$": "dockerhub/$1"
```

Corresponding `remotes.yaml` entry:

```yaml
remotes:
  dockerhub:
    base_url: "https://registry-1.docker.io"
    type: "remote"
    package: "docker"
    username: "your-dockerhub-username"
    password: "your-dockerhub-token"   # PAT with read scope
    cache:
      immutable_ttl: 0
      mutable_ttl: 300
```

A pull of `nginx:latest` becomes `/v2/dockerhub/library/nginx/manifests/latest` on the artifact API.

#### GitHub Container Registry (ghcr.io)

```yaml
mirrors:
  ghcr.io:
    endpoint:
      - "https://artifacts.example.com"
    rewrite:
      "^(.*)$": "ghcr/$1"
```

```yaml
remotes:
  ghcr:
    base_url: "https://ghcr.io"
    type: "remote"
    package: "docker"
    username: "your-github-username"
    password: "ghp_your_github_pat"   # read:packages scope required
    cache:
      immutable_ttl: 0
      mutable_ttl: 300
```

A pull of `ghcr.io/rancher/rke2-runtime:v1.30.0-rke2r1` becomes `/v2/ghcr/rancher/rke2-runtime/manifests/v1.30.0-rke2r1`.

#### Multiple registries

```yaml
# /etc/rancher/rke2/registries.yaml
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

  registry.k8s.io:
    endpoint:
      - "https://artifacts.example.com"
    rewrite:
      "^(.*)$": "k8s-registry/$1"

  quay.io:
    endpoint:
      - "https://artifacts.example.com"
    rewrite:
      "^(.*)$": "quay/$1"
```

Each entry needs a matching remote in `remotes.yaml` using the name from the rewrite target (e.g. `k8s-registry`, `quay`).

#### Restricting which images are cached

Use `immutable_patterns` on the remote to allow only specific images through the proxy. Requests for images not matching any pattern return HTTP 403 to the node.

```yaml
remotes:
  dockerhub:
    base_url: "https://registry-1.docker.io"
    type: "remote"
    package: "docker"
    immutable_patterns:
      - "^library/nginx"           # official nginx only
      - "^library/redis"           # official redis only
      - "^rancher/"                # all rancher images
      - "^grafana/grafana"         # specific image
    cache:
      immutable_ttl: 0
      mutable_ttl: 300
```

Omit `immutable_patterns` to allow all images from that registry.

#### TLS configuration

If the artifact API uses a private CA certificate, tell containerd about it in `registries.yaml`:

```yaml
mirrors:
  docker.io:
    endpoint:
      - "https://artifacts.example.com"
    rewrite:
      "^(.*)$": "dockerhub/$1"

configs:
  "artifacts.example.com":
    tls:
      ca_file: /etc/ssl/certs/internal-ca.crt
```

### Applying the configuration

```bash
# Write registries.yaml on each node (server and agent)
sudo mkdir -p /etc/rancher/rke2
sudo tee /etc/rancher/rke2/registries.yaml <<'EOF'
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
EOF

# Restart the RKE2 service (server nodes)
sudo systemctl restart rke2-server

# Or on agent nodes
sudo systemctl restart rke2-agent

# Confirm containerd picked up the mirror config
sudo /var/lib/rancher/rke2/bin/crictl info | jq '.config.registry.mirrors'
```

### Verifying pulls go through the cache

```bash
# Pull an image on a node
sudo /var/lib/rancher/rke2/bin/crictl pull nginx:latest

# Check the artifact API received the request
kubectl logs deployment/artifactapi -n artifact-storage | grep "nginx"
# Expect: Cache MISS on first pull, Cache HIT on subsequent pulls

# Query the manifest endpoint directly — 200 means it's cached
curl -I https://artifacts.example.com/v2/dockerhub/library/nginx/manifests/latest

# Check what's stored in the cache
curl https://artifacts.example.com/ | jq '.remotes'
```