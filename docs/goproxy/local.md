# Local Go module proxy

A local `goproxy` repo serves private modules over the GOPROXY protocol
(`@v/list`, `@latest`, `@v/<ver>.info|.mod|.zip`). Publish a version by
uploading its files under the case-encoded module path (uppercase as `!lower`):

```sh
for ext in zip mod info; do
  curl -fsSL --upload-file v0.1.0.$ext \
    https://artifactapi.example.com/api/v2/remotes/go-internal/files/myorg.example.net/hello/@v/v0.1.0.$ext
done
GOPROXY=https://artifactapi.example.com/api/v1/local/go-internal,https://proxy.golang.org,direct \
GONOSUMDB=myorg.example.net go get myorg.example.net/hello@latest
```

Build the zip with `golang.org/x/mod/zip` or reuse the files `go mod download`
leaves in `$(go env GOMODCACHE)/cache/download`. Uploads are validated: zips must
pass `zip.CheckZip` (every file under `module@version/`), `.mod` must declare the
module, and `.info` must match the version. `.info` is optional; it is
synthesized from the version when absent. `@v/list` lists versions with a `.mod`.
