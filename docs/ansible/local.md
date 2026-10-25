# Local ansible repository

A local `ansible` repo is a Galaxy v3 server: `ansible-galaxy collection publish`
and `ansible-galaxy collection install` work against `/api/v1/local/<repo>`.
`publish` needs an API key; any value works unless [uploads are gated](../common/auth.md).

```bash
ansible-galaxy collection build
ansible-galaxy collection publish myorg-hello-1.0.0.tar.gz \
  --server https://artifactapi.example.com/api/v1/local/<repo>/ --api-key unused
ansible-galaxy collection install myorg.hello \
  --server https://artifactapi.example.com/api/v1/local/<repo>/
```

Publishing reads `collection_info` from the tarball's `MANIFEST.json` and
finishes the import synchronously, so the import task is already `completed`.
Uploads over 256 MiB, tarballs with members or symlinks escaping the collection root, or
with an invalid name, version or dependency map are rejected with 400.

The repo serves `api/v3/collections/<ns>/<name>/`, the paginated
`versions/` list (`limit` up to 100, `offset`), `versions/<version>/` with
`download_url`, `artifact.sha256` and `metadata.dependencies`, and
`artifacts/<ns>-<name>-<version>.tar.gz`.

Versions are immutable: publishing an existing `namespace.name:version` is
rejected with 409.
