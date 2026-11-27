package provider

import "github.com/IUCLabs/artifactapi/pkg/models"

// ResponseRewriter rewrites a mutable remote response once, before it is
// cached, so its links must not depend on the requesting host.
type ResponseRewriter interface {
	RewriteResponse(body []byte, remote models.Remote) ([]byte, error)
}
