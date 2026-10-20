# `puppet` virtual

A puppet virtual serves one Forge v3 API over local and remote puppet members:
`puppet module install` and r10k work against `/api/v1/virtual/<name>`, with
dependencies resolved across members.

- `/v3/releases?module=<slug>` merges member releases; on a duplicate version
  the earlier member wins. Releases are newest first, paginated with
  `limit` (up to 100) and `offset`; pagination links and `file_uri` are
  relative to the Forge root, so downloads route via the virtual.
- `/v3/modules/<slug>` lists the merged releases with the highest version as
  `current_release`.
- `/v3/releases/<slug>-<version>` and `/v3/files/<file>` 302 to the member
  owning that release.
- A member error fails the request with 502; a member without the module is
  skipped. Merges, and modules no member has, are cached for 60s.

```hcl
resource "artifactapi_virtual" "puppet" {
  name         = "puppet"
  package_type = "puppet"
  members      = [artifactapi_local_puppet.internal.name, artifactapi_remote_puppet.forge.name]
}
```

```bash
puppet module install myorg-hello \
  --module_repository https://artifactapi.example.com/api/v1/virtual/puppet
```

```yaml
# r10k.yaml
forge:
  baseurl: https://artifactapi.example.com/api/v1/virtual/puppet
```

Upload to the local member directly; the virtual is read-only.
