package provider

import (
	"github.com/IUCLabs/artifactapi/pkg/models"
)

// ReleasesRewriter rewrites a cached mutable remote response per request, like
// ServeRewriter, but also needs the config of the remote's releases_remote.
type ReleasesRewriter interface {
	RewriteReleases(body []byte, remote, releases models.Remote, proxyBaseURL string) []byte
}
