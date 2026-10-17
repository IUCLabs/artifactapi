# `helm` virtual

```hcl
resource "artifactapi_virtual" "helm" {
  name         = "helm"
  package_type = "helm"
  members      = [artifactapi_remote_helm.jetstack.name]
}
```
