# `npm` virtual

An npm virtual serves one registry over local and remote npm members:
`npm install` and `npm view` work against `/api/v1/virtual/<name>/`.

- Versions are the union across members; on a duplicate version the earlier
  member wins.
- Each dist-tag comes from the first member defining it, and is dropped if it
  names a version missing from the merged set.
- Tarball URLs point at the virtual (`<member>/<path>`), which 302s to the
  owning member.
- Full and abbreviated (`application/vnd.npm.install-v1+json`) packuments are
  both served.
- A member error fails the request with 502; a member without the package is
  skipped. Merges are cached for 60s per package.

```hcl
resource "artifactapi_virtual" "npm" {
  name         = "npm"
  package_type = "npm"
  members      = [artifactapi_local_npm.internal.name, artifactapi_remote_npm.npmjs.name]
}
```

```ini
# .npmrc
registry=https://artifactapi.example.com/api/v1/virtual/npm/
```

Publish to the local member directly; the virtual is read-only.
