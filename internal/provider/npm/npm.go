package npm

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/IUCLabs/artifactapi/internal/auth"
	"github.com/IUCLabs/artifactapi/internal/provider"
	"github.com/IUCLabs/artifactapi/pkg/models"
)

func init() {
	provider.Register(&Provider{})
}

type Provider struct{}

func (p *Provider) Type() models.PackageType { return models.PackageNPM }

func (p *Provider) Classify(path string) provider.Mutability {
	if strings.HasSuffix(path, ".tgz") {
		return provider.Immutable
	}
	return provider.Mutable
}

func (p *Provider) ContentType(path string) string {
	if strings.HasSuffix(path, ".tgz") {
		return "application/gzip"
	}
	return "application/json"
}

func (p *Provider) UpstreamURL(remote models.Remote, path string) string {
	return strings.TrimRight(remote.BaseURL, "/") + "/" + strings.TrimLeft(path, "/")
}

// RewriteServed points tarball URLs in cached packuments at this remote, so
// tarballs are fetched and cached here.
func (p *Provider) RewriteServed(body []byte, remote models.Remote, proxyBaseURL string) []byte {
	baseURL := strings.TrimRight(remote.BaseURL, "/")
	if baseURL == "" || !json.Valid(body) {
		return body
	}
	proxyURL := strings.TrimRight(proxyBaseURL, "/") + provider.RemoteMountPath + remote.Name
	return []byte(strings.ReplaceAll(string(body), baseURL+"/", proxyURL+"/"))
}

func (p *Provider) AuthHeaders(_ context.Context, remote models.Remote) (http.Header, error) {
	return auth.BasicHeaders(remote), nil
}
