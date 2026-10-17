# Local puppet forge

A local `puppet` repo is a Forge: `PUT` a module tarball to
`/api/v2/remotes/<repo>/files/<owner>-<module>-<version>.tar.gz` and the Forge
v3 API (`/v3/modules`, `/v3/releases`, `/v3/files`) is served from each
release's `metadata.json` at `/api/v1/local/<repo>`. Versions are immutable.

```bash
puppet module build ./hello
curl -fsSL --upload-file pkg/myorg-hello-0.1.0.tar.gz \
  https://artifactapi.example.com/api/v2/remotes/<repo>/files/myorg-hello-0.1.0.tar.gz
puppet module install myorg-hello \
  --module_repository https://artifactapi.example.com/api/v1/local/<repo>
```
