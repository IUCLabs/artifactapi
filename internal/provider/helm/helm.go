package helm

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

func (p *Provider) Type() models.PackageType { return models.PackageHelm }

func (p *Provider) Classify(path string) provider.Mutability {
	if strings.HasSuffix(path, "index.yaml") || strings.HasSuffix(path, "index.yml") {
		return provider.Mutable
	}
	return provider.Immutable
}

func (p *Provider) ContentType(path string) string {
	if strings.HasSuffix(path, ".tgz") || strings.HasSuffix(path, ".tar.gz") {
		return "application/gzip"
	}
	if strings.HasSuffix(path, ".yaml") || strings.HasSuffix(path, ".yml") {
		return "text/yaml"
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
	baseURL := strings.TrimRight(remote.BaseURL, "/")
	proxyURL := strings.TrimRight(proxyBaseURL, "/") + "/api/v1/remote/" + remote.Name
	rewritten := strings.ReplaceAll(content, baseURL, proxyURL)
	if rewritten == content {
		return nil, nil
	}
	return []byte(rewritten), nil
}

func (p *Provider) AuthHeaders(_ context.Context, remote models.Remote) (http.Header, error) {
	return auth.BasicHeaders(remote), nil
}
