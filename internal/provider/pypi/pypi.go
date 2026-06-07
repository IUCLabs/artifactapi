package pypi

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

func (p *Provider) Type() models.PackageType { return models.PackagePyPI }

func (p *Provider) Classify(path string) provider.Mutability {
	if strings.Contains(path, "simple/") {
		return provider.Mutable
	}
	return provider.Immutable
}

func (p *Provider) ContentType(path string) string {
	lower := strings.ToLower(path)
	if strings.HasSuffix(lower, ".whl") || strings.HasSuffix(lower, ".zip") {
		return "application/zip"
	}
	if strings.HasSuffix(lower, ".tar.gz") {
		return "application/gzip"
	}
	if strings.Contains(path, "simple/") {
		return "text/html"
	}
	return "application/octet-stream"
}

func (p *Provider) UpstreamURL(remote models.Remote, path string) string {
	if strings.HasPrefix(path, "simple/") {
		return "https://pypi.org/" + path
	}
	return strings.TrimRight(remote.BaseURL, "/") + "/" + strings.TrimLeft(path, "/")
}

func (p *Provider) RewriteResponse(body []byte, remote models.Remote, proxyBaseURL string) ([]byte, error) {
	if proxyBaseURL == "" {
		return nil, nil
	}
	content := string(body)
	proxyURL := strings.TrimRight(proxyBaseURL, "/") + "/api/v1/remote/" + remote.Name + "/"
	content = strings.ReplaceAll(content, "https://files.pythonhosted.org/", proxyURL)
	content = strings.ReplaceAll(content, "../../", proxyURL)
	return []byte(content), nil
}

func (p *Provider) AuthHeaders(_ context.Context, remote models.Remote) (http.Header, error) {
	return auth.BasicHeaders(remote), nil
}
