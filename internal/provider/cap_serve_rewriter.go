package provider

import (
	"github.com/IUCLabs/artifactapi/pkg/models"
)

// ServeRewriter rewrites a cached mutable remote response per request, for
// indexes whose URLs must name the requesting host and so cannot be rewritten
// before caching.
type ServeRewriter interface {
	RewriteServed(body []byte, remote models.Remote, proxyBaseURL string) []byte
}
