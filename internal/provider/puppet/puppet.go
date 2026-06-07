package puppet

import (
	"context"
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

func (p *Provider) Type() models.PackageType { return models.PackagePuppet }

func (p *Provider) Classify(path string) provider.Mutability {
	if strings.HasPrefix(path, "v3/modules/") || strings.HasPrefix(path, "v3/releases") {
		return provider.Mutable
	}
	return provider.Immutable
}

func (p *Provider) ContentType(path string) string {
	if strings.HasSuffix(path, ".tar.gz") {
		return "application/gzip"
	}
	if strings.HasPrefix(path, "v3/") {
		return "application/json"
	}
	return "application/octet-stream"
}

func (p *Provider) UpstreamURL(remote models.Remote, path string) string {
	return strings.TrimRight(remote.BaseURL, "/") + "/" + strings.TrimLeft(path, "/")
}

func (p *Provider) RewriteResponse(body []byte, remote models.Remote, proxyBaseURL string) ([]byte, error) {
	if proxyBaseURL == "" {
		return nil, nil
	}
	content := string(body)
	proxyURL := strings.TrimRight(proxyBaseURL, "/") + "/api/v1/remote/" + remote.Name
	content = strings.ReplaceAll(content, `"/v3/files/`, `"`+proxyURL+`/v3/files/`)
	baseURL := strings.TrimRight(remote.BaseURL, "/")
	content = strings.ReplaceAll(content, baseURL, proxyURL)
	return []byte(content), nil
}

func (p *Provider) AuthHeaders(_ context.Context, remote models.Remote) (http.Header, error) {
	return auth.BasicHeaders(remote), nil
}
