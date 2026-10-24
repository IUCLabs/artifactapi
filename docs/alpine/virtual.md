# `alpine` virtual

An alpine virtual serves one apk repository over local and remote alpine
members at `/api/v1/virtual/<name>/`.

- `<dir>/APKINDEX.tar.gz` merges each member's index for the same `<dir>`
  (e.g. `x86_64/`); on a duplicate (P, V, A) the earlier member wins. A member
  that 404s for that directory is skipped.
- apk builds download paths itself (`<dir>/<P>-<V>.apk`), so the virtual 302s
  each download to the member whose index listed it.
- The merged index is unsigned, like local and `github_alpine` indexes: clients
  need `--allow-untrusted` (packages keep their own signatures).
- Any other member error fails the request with 502. Merges are cached for 60s
  per directory, up to 32 directories per virtual.

```hcl
resource "artifactapi_virtual" "alpine" {
  name         = "alpine"
  package_type = "alpine"
  members      = [artifactapi_local_alpine.internal.name, artifactapi_remote_github_alpine.tool.name]
}
```

```sh
echo https://artifactapi.example.com/api/v1/virtual/alpine >> /etc/apk/repositories
apk update --allow-untrusted
apk add --allow-untrusted mytool
```

Upload to the local member directly; the virtual is read-only.
