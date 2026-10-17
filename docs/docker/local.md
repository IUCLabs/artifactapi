# Local docker registry

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
