# Artifact Storage System

A generic FastAPI-based artifact caching system that downloads and stores files from remote sources (GitHub, Gitea, HashiCorp, etc.) in S3-compatible storage with configuration-based access control.

## Features

- **Generic Remote Support**: Works with any HTTP-based file server (GitHub, Gitea, HashiCorp, custom servers)
- **Configuration-Based**: YAML configuration for remotes, patterns, and access control
- **Direct URL API**: Access cached files via clean URLs like `/api/github/owner/repo/path/file.tar.gz`
- **Pattern Filtering**: Regex-based inclusion patterns for security and organization
- **Smart Caching**: Automatic download and cache on first access, serve from cache afterward
- **S3 Storage**: MinIO/S3 backend with predictable paths
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
    type: "remote"                        # Type: "remote" or "local"
    package: "generic"                    # Package type: "generic", "alpine", "rpm"
    description: "Human readable description"
    include_patterns:                     # Regex patterns for allowed files
      - "pattern1"
      - "pattern2"
    cache:                               # Cache configuration (optional)
      file_ttl: 0                        # File cache TTL (0 = indefinite)
      index_ttl: 300                     # Index file TTL in seconds
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
    include_patterns:
      - "gruntwork-io/terragrunt/.*terragrunt_linux_amd64.*"
      - "lxc/incus/.*\\.tar\\.gz$"
      - "prometheus/node_exporter/.*/node_exporter-.*\\.linux-amd64\\.tar\\.gz$"
    cache:
      file_ttl: 0      # Cache files indefinitely
      index_ttl: 0     # No index files for generic remotes

  hashicorp-releases:
    base_url: "https://releases.hashicorp.com"
    type: "remote"
    package: "generic"
    description: "HashiCorp product releases"
    include_patterns:
      - "terraform/.*terraform_.*_linux_amd64\\.zip$"
      - "vault/.*vault_.*_linux_amd64\\.zip$"
      - "consul/.*/consul_.*_linux_amd64\\.zip$"
    cache:
      file_ttl: 0
      index_ttl: 0
```

#### Package Repository Remotes
For Linux package repositories with index files:

```yaml
remotes:
  alpine:
    base_url: "https://dl-cdn.alpinelinux.org"
    type: "remote"
    package: "alpine"
    description: "Alpine Linux APK package repository"
    include_patterns:
      - ".*/x86_64/.*\\.apk$"           # Only x86_64 packages
    cache:
      file_ttl: 0                       # Cache packages indefinitely
      index_ttl: 7200                   # Cache APKINDEX.tar.gz for 2 hours

  almalinux:
    base_url: "http://mirror.aarnet.edu.au/pub/almalinux"
    type: "remote"
    package: "rpm"
    description: "AlmaLinux RPM package repository"
    include_patterns:
      - ".*/x86_64/.*\\.rpm$"
      - ".*/noarch/.*\\.rpm$"
    cache:
      file_ttl: 0
      index_ttl: 7200                   # Cache metadata files for 2 hours
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
      file_ttl: 0
      index_ttl: 0
```

### Include Patterns

Include patterns are regular expressions that control which files can be accessed:

```yaml
include_patterns:
  # Specific project patterns
  - "gruntwork-io/terragrunt/.*terragrunt_linux_amd64.*"

  # File extension patterns
  - ".*\\.tar\\.gz$"
  - ".*\\.zip$"
  - ".*\\.rpm$"

  # Architecture-specific patterns
  - ".*/x86_64/.*"
  - ".*/linux-amd64/.*"

  # Version-specific patterns
  - "prometheus/node_exporter/.*/node_exporter-.*\\.linux-amd64\\.tar\\.gz$"
```

**Security Note**: Only files matching at least one include pattern are accessible. Files not matching any pattern return HTTP 403.

### Cache Configuration

Control how long different file types are cached:

```yaml
cache:
  file_ttl: 0        # Regular files (0 = cache indefinitely)
  index_ttl: 300     # Index files like APKINDEX.tar.gz (seconds)
```

**Index Files**: Repository metadata files that change frequently:
- Alpine: `APKINDEX.tar.gz`
- RPM: `repomd.xml`, `*-primary.xml.gz`, etc.
- These are automatically detected and use `index_ttl`

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
        include_patterns:
          - "gruntwork-io/terragrunt/.*terragrunt_linux_amd64.*"
          - "lxc/incus/.*\\.tar\\.gz$"
          - "prometheus/node_exporter/.*/node_exporter-.*\\.linux-amd64\\.tar\\.gz$"
        cache:
          file_ttl: 0
          index_ttl: 0

      hashicorp-releases:
        base_url: "https://releases.hashicorp.com"
        type: "remote"
        package: "generic"
        description: "HashiCorp product releases"
        include_patterns:
          - "terraform/.*terraform_.*_linux_amd64\\.zip$"
          - "vault/.*vault_.*_linux_amd64\\.zip$"
          - "consul/.*/consul_.*_linux_amd64\\.zip$"
        cache:
          file_ttl: 0
          index_ttl: 0
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