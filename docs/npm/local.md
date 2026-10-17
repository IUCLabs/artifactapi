# Local npm registry

A local `npm` repo is a real npm registry: `npm publish` (scoped and unscoped)
and `npm install` work against `/api/v1/local/<repo>/`. npm requires an
`_authToken` for the registry before it will publish; any value works.

```ini
# .npmrc
registry=https://artifactapi.example.com/api/v1/local/<repo>/
//artifactapi.example.com/api/v1/local/<repo>/:_authToken=unused
```

Versions are immutable: republishing an existing version is rejected. The
dist-tags sent with a publish (`latest`, or `--tag`) move to that version.
