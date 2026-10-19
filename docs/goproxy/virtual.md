# `goproxy` virtual

A goproxy virtual serves one GOPROXY over local and remote goproxy members:
`GOPROXY=https://artifactapi.example.com/api/v1/virtual/<name>`.

- `<module>/@v/list` is the union of member versions, sorted by semver.
- `<module>/@latest` is the highest release in that union, else the highest
  prerelease; with no listed versions it falls back to the first member that
  answers `@latest`.
- `@latest` and `@v/<version>.{info,mod,zip}` 302 to the first member, in
  member order, that lists the version (or, for unlisted pseudo-versions,
  serves its `.info`). Redirects are root-relative and `no-store`.
- Module paths are case-encoded (uppercase as `!lower`).
- A member error fails the request with 502; a member answering 404 or 410 is
  skipped. Merged lists are cached for 60s.

```hcl
resource "artifactapi_virtual" "go" {
  name         = "go"
  package_type = "goproxy"
  members      = [artifactapi_local_goproxy.internal.name, artifactapi_remote_goproxy.goproxy.name]
}
```

```sh
export GOPROXY=https://artifactapi.example.com/api/v1/virtual/go GONOSUMDB=myorg.example.net
go get myorg.example.net/hello@latest golang.org/x/text@latest
```

Publish to the local member directly; the virtual is read-only.
