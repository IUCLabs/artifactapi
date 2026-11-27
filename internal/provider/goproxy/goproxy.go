package goproxy

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

func (p *Provider) Type() models.PackageType { return models.PackageGoProxy }

func (p *Provider) Classify(path string) provider.Mutability {
	if strings.HasSuffix(path, "/@v/list") || strings.HasSuffix(path, "/@latest") {
		return provider.Mutable
	}
	return provider.Immutable
}

func (p *Provider) ContentType(path string) string {
	if strings.HasSuffix(path, ".zip") {
		return "application/zip"
	}
	if strings.HasSuffix(path, ".mod") {
		return "text/plain"
	}
	if strings.HasSuffix(path, ".info") {
		return "application/json"
	}
	if strings.HasSuffix(path, "/list") {
		return "text/plain"
	}
	return "application/octet-stream"
}

func (p *Provider) UpstreamURL(remote models.Remote, path string) string {
	return strings.TrimRight(remote.BaseURL, "/") + "/" + strings.TrimLeft(path, "/")
}

func (p *Provider) AuthHeaders(_ context.Context, remote models.Remote) (http.Header, error) {
	return auth.BasicHeaders(remote), nil
}
