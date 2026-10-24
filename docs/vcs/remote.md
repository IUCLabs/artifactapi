# `vcs` remote

Shared classification, TTL and eviction rules: [caching](../common/caching.md).

Caches source archives and release assets from GitHub (`base_url = "https://github.com"`) or Gitea (any other `base_url`). GitHub Enterprise is not supported: a non-github.com host is treated as Gitea. Only the paths below are served; anything else is a 404.

| Path | Behaviour |
|---|---|
| `<owner>/<repo>/archive/refs/tags/<tag>.(tar.gz\|zip)` | immutable |
| `<owner>/<repo>/archive/<40-char sha>.(tar.gz\|zip)` | immutable |
| `<owner>/<repo>/releases/download/<tag>/<asset>` | immutable |
| `<owner>/<repo>/releases/latest/download/<asset>` | mutable |
| `<owner>/<repo>/archive/refs/heads/<branch>.(tar.gz\|zip)` | `302` to the archive of the branch's current commit, resolved via the provider's branch API and reused for `mutable_ttl` |

Branch archives redirect, so `curl` needs `-L`; pip and npm follow redirects. The archive is the host's commit archive (GitHub names its top-level directory `<repo>-<sha>`). `patterns` and `blocklist` match both the branch path and the commit path it redirects to, so allowlist repos by prefix (`^cli/cli/`); a branch whose commit path is denied is a `403`. A missing branch is a `404`, cached for `mutable_ttl` (at least 60s). An API rate limit (`429`, or `403` with rate-limit headers) is a `503` with `Retry-After`, unless `stale_on_error` serves the last known commit. Each remote caches up to 4096 branch lookups. GitHub API calls use the [server GitHub credential](../../README.md#github-authentication); Gitea API calls use the remote's `username`/`password`.

```hcl
resource "artifactapi_remote_vcs" "github" {
  name        = "github-vcs"
  base_url    = "https://github.com"
  mutable_ttl = 600
  patterns    = ["^cli/cli/", "^psf/requests/"]
}
```

```bash
curl -fsSLO https://artifactapi.example/api/v1/remote/github-vcs/cli/cli/archive/refs/tags/v2.60.0.tar.gz
pip install https://artifactapi.example/api/v1/remote/github-vcs/psf/requests/archive/refs/tags/v2.32.3.tar.gz
```
