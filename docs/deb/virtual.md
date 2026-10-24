# `deb` virtual

A deb virtual serves one apt source over local and remote deb members at
`/api/v1/virtual/<name>/`, in either the flat (`./`) or `dists/<suite>` layout.

- Packages indexes are merged; on a duplicate (Package, Version, Architecture)
  the earlier member wins.
- The Release is generated unsigned with `Acquire-By-Hash: yes`; indexes are
  served by name and by SHA256 from the current and previous merge.
- `Filename` points at the virtual (`<member>/<path>`), which 302s to the
  owning member.
- All members must share one layout; mixing flat and suite members is
  unsupported (suite requests fail with 502).
- A member error fails the request with 502. Merges are cached for 60s per
  suite, up to 32 suites per virtual.

```hcl
resource "artifactapi_virtual" "deb" {
  name         = "deb"
  package_type = "deb"
  members      = [artifactapi_local_deb.internal.name, artifactapi_remote_github_deb.tool.name]
}
```

```
# /etc/apt/sources.list.d/artifactapi.list (flat)
deb [trusted=yes] https://artifactapi.example.com/api/v1/virtual/deb/ ./
# suite layout
deb [trusted=yes] https://artifactapi.example.com/api/v1/virtual/debian/ trixie main
```

Upload to the local member directly; the virtual is read-only.
