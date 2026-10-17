# `helm` remote

Shared classification, TTL and eviction rules: [caching](../common/caching.md).

| Type | Mutable (auto-detected) | Immutable (auto-detected) |
|---|---|---|
| `helm` | `index.yaml` | `.tgz` charts |

## Terraform

```hcl
resource "artifactapi_remote_helm" "jetstack" {
  name     = "jetstack"
  base_url = "https://charts.jetstack.io"

  immutable_ttl = 0
  mutable_ttl   = 3600
}
```
