# `ansible` virtual

An ansible virtual serves one Galaxy v3 API over local and remote ansible
members: `ansible-galaxy collection install` works against
`/api/v1/virtual/<name>/`.

- `versions/` concatenates member version lists in member order; on a
  duplicate version the earlier member wins. Pagination (`limit` up to 100,
  `offset`) runs over the merged list.
- `versions/<version>/` is the owning member's detail with `href` and
  `download_url` pointing at the virtual; the download 302s to that member.
- A member error fails the request with 502; a member without the collection
  is skipped. Merges are cached for 60s.

```hcl
resource "artifactapi_virtual" "ansible" {
  name         = "ansible"
  package_type = "ansible"
  members      = [artifactapi_local_ansible.internal.name, artifactapi_remote_ansible.galaxy.name]
}
```

```bash
ansible-galaxy collection install myorg.hello community.general \
  --server https://artifactapi.example.com/api/v1/virtual/ansible/
```

Publish to the local member directly; the virtual is read-only.
