# `generic` remote

Shared classification, TTL and eviction rules: [caching](../common/caching.md).

| Type | Mutable (auto-detected) | Immutable (auto-detected) |
|---|---|---|
| `generic` | nothing | everything |

## Terraform

```hcl
resource "artifactapi_remote_generic" "github" {
  name     = "github"
  base_url = "https://github.com"

  immutable_ttl = 0
  mutable_ttl   = 7200

  patterns = [
    "ducaale/xh/.*/xh-.*-x86_64-unknown-linux-musl.tar.gz$",
    "mikefarah/yq/.*/yq_linux_amd64$",
  ]

  mutable_patterns = [
    ".*/archive/refs/heads/.*\\.tar\\.gz$",
  ]
}
```
