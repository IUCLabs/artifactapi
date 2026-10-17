# Serving providers as a registry

A local `terraform` repo is a real provider registry: upload
`terraform-provider-{type}_{version}_{os}_{arch}.zip` files under
`{namespace}/{type}/`, and Terraform installs them from a bare source address —
no `.terraformrc` mirror config:

```hcl
terraform {
  required_providers {
    artifactapi = {
      source  = "artifactapi.example.com/<repo>/<type>"
      version = "0.1.2"
    }
  }
}
```

The Terraform *namespace* segment is the artifactapi repo name; the provider is
matched by *type*. The registry serves service discovery
(`/.well-known/terraform.json`), the `providers.v1` version/download endpoints,
and a GPG-signed `SHA256SUMS` per the provider registry protocol.

Signing needs a GPG key. By default artifactapi generates one on first start and
stores it in the database (`signing_keys` table), so every replica shares it and
there's nothing to provision. To bring your own key instead, point
`TF_SIGNING_KEY_PATH` at an armored private key (optionally
`TF_SIGNING_KEY_PASSPHRASE`), which takes precedence over the generated one.
`TF_PROVIDER_PROTOCOLS` (default `5.0,6.0`) sets the advertised plugin protocols.
