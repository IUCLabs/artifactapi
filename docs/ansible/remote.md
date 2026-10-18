# `ansible` remote

Shared classification, TTL and eviction rules: [caching](../common/caching.md).

| Type | Mutable (auto-detected) | Immutable (auto-detected) |
|---|---|---|
| `ansible` | Galaxy `api/` JSON (links rewritten, `limit`/`offset` forwarded) | `artifacts/*.tar.gz` |

```bash
ansible-galaxy collection install community.general -s https://artifactapi.example/api/v1/remote/galaxy/api/
```
