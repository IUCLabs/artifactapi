# Local helm repository

A local `helm` repo is a real chart repository: `PUT` a packaged chart to
`/api/v2/remotes/<repo>/files/<chart>-<version>.tgz` and `index.yaml` is
generated from each chart's `Chart.yaml` (with sha256 digests) at
`/api/v1/local/<repo>/index.yaml`; a file not named after its `Chart.yaml`
name and version is stored but left out of the index. Add the local as a
member of a helm virtual to serve it alongside remotes.

```bash
helm package ./mychart
curl -fsSL --upload-file mychart-0.1.0.tgz \
  https://artifactapi.example.com/api/v2/remotes/<repo>/files/mychart-0.1.0.tgz
helm repo add <repo> https://artifactapi.example.com/api/v1/local/<repo>
```
