# Write authentication

artifactapi verifies writes against an OIDC issuer's JWKS, such as a Vault
identity OIDC role. Reads, probes and anonymous pulls never need a token.

| Variable | Gates |
|---|---|
| `ARTIFACTAPI_OIDC_ISSUER` + `ARTIFACTAPI_OIDC_AUDIENCE` | creating, updating and deleting remotes and virtuals |
| `ARTIFACTAPI_AUTH_UPLOADS=true` | also every local repo write: `PUT`/`DELETE /api/v2/remotes/<name>/files/*`, `DELETE /api/v2/{remotes,locals}/<name>/objects/*`, `PUT`/`POST`/`DELETE /api/v1/local/<repo>/*` (npm publish, gem push/yank, ansible-galaxy publish) and Docker Registry V2 push/delete under `/v2` |

Any token from the issuer whose `aud` is in `ARTIFACTAPI_OIDC_AUDIENCE` may
write to every repo.

## Minting a token

```sh
vault read -field=token identity/oidc/token/<role> > token
```

The role's `client_id` must be one of the configured audiences.

## Passing the token

The token is accepted as `Authorization: Bearer <token>`, as `Token <token>`,
as the password of HTTP Basic credentials with any username, or as a bare
header value.

```sh
# curl
curl -fsSL -H "Authorization: Bearer $(cat token)" --upload-file f.tgz \
  https://artifactapi.example.com/api/v2/remotes/<repo>/files/f.tgz
curl -fsSL -u "ci:$(cat token)" --upload-file f.tgz \
  https://artifactapi.example.com/api/v2/remotes/<repo>/files/f.tgz

# docker / podman / helm (OCI): any username, token as password
docker login artifactapi.example.com -u ci --password-stdin < token
helm registry login artifactapi.example.com -u ci --password-stdin < token

# npm (.npmrc)
//artifactapi.example.com/api/v1/local/<repo>/:_authToken=<token>

# gem
GEM_HOST_API_KEY="$(cat token)" gem push --host https://artifactapi.example.com/api/v1/local/<repo> mygem-1.0.0.gem

# ansible-galaxy
ansible-galaxy collection publish c.tar.gz \
  --server https://artifactapi.example.com/api/v1/local/<repo>/ --api-key "$(cat token)"
```

With uploads gated, `/v2/` answers anonymous requests with a Bearer challenge
pointing at `/v2/token`. That endpoint returns the verified Basic password as
the registry token, or an anonymous token that only reads, so `docker pull`
keeps working without a login.
