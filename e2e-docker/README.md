# Dockerised end-to-end suite

Black-box tests that run against a fully **containerised** artifactapi stack
(built image + Postgres + Redis + MinIO) plus a static mock upstream. Unlike the
in-process `e2e/` suite (testcontainers, server run in-process), these only speak
HTTP to the running product, so they exercise the shipped container image.

## Run

```bash
make docker-e2e          # build image, compose up, run suite, compose down
```

`scripts/docker-e2e.sh` builds and starts `docker-compose.yml` +
`docker-compose.e2e.yml`, waits for `/health`, then runs
`go test -tags=dockere2e ./e2e-docker/...` and tears everything down.

The stack publishes artifactapi on host port **8001** (to avoid colliding with a
local instance on 8000). Override with `ARTIFACTAPI_URL` to point the tests at an
already-running stack.

## Coverage

- **Write auth** — the stack runs with `ARTIFACTAPI_AUTH_UPLOADS=true` against a
  static `mockoidc` issuer (`oidc/`); `TestMain` generates the signing key, writes
  `oidc/jwks.json` and mints the tokens. Every local publish authenticates the way
  its real client does (curl Bearer/Basic, `docker login`, npm `_authToken`,
  `GEM_HOST_API_KEY`, `ansible-galaxy --api-key`), and each type checks a
  publish without a token or with a foreign-key token is refused while
  anonymous reads still work.
- **Repository lifecycle** — add / change / delete for remote, local and virtual repos.
- **Caching** — one immutable artifact per remote package type (one
  `caching_<type>_test.go` case each) proxied through the mock upstream: first fetch `X-Artifact-Source: remote`, second `cache`, bytes
  verified against the origin fixture.
- **Local uploads** — puppet (Forge v3 API via real `r10k` and `puppet module install`),
  generic (upload/download), pypi (wheel + generated `simple/`
  index), rpm (real package + **automatic repodata** generation), npm (stock
  `node:22` client: `npm publish` unscoped + scoped, `npm install` + `require()`,
  second version, republish rejected), gems (stock `ruby:3.3`: `gem push`,
  `gem install`, `bundle install` + `require`, republish rejected, `gem yank`,
  yanked re-push rejected), ansible (stock `ansible-galaxy`: `collection build`,
  `publish`, republish rejected, `install` + module run).
- **Virtual repositories** — pypi simple-index merge and helm `index.yaml` merge
  across two members; npm packument merge over two locals via a stock `node:22`
  client (merged versions, install through the member redirect); gems compact
  index merge over two locals via stock `ruby:3.3` bundler (member order wins,
  downloads 302 to the owning member); ansible over two locals via stock
  `ansible-galaxy` (install through the virtual, download 302 to the owner);
  goproxy over two locals via a stock `golang:1.25` client (merged versions,
  member order wins, version files 302 to the owning member); puppet over two
  locals via real `puppet module install` (dependency resolved across members)
  and `r10k`, downloads 302 to the owning member.
- **Mirrorlist** — an rpm remote with a `mirrorlist` of extra upstream mirrors
  (pool = `base_url` + `mirrorlist`): round-robin distribution across both mirrors
  (constant-body `mockupstreama` / `mockupstreamb`), failover past a dead primary,
  no-mirrorlist regression, and a real `dnf` (stock `rockylinux:9` container)
  `makecache` + `install` through a two-mirror rpm remote whose `base_url` is dead
  — a dead mirror must not break the client.
- **Mirror strategy (`least_conn`)** — a `mirror_strategy: least_conn` rpm remote
  over the two constant-body mirrors exercises the least-connections selection
  path end-to-end (both mirrors serve, all requests succeed), plus a real `dnf`
  install through a `least_conn` remote with a dead primary (failover unchanged).
  The precise least-loaded pick is asserted deterministically in the proxy unit
  test, since an in-flight-skew assertion over HTTP is timing-sensitive.

## Fixtures

`fixtures/` is served by the mock upstream at its web root. Paths mirror each
provider's upstream URL layout (e.g. `v2/...` for docker, `v1/providers/...` for
terraform). The RPM under `fixtures/rpmrepo/Packages/` is a real package so the
rpm provider can parse its metadata for repodata generation.
