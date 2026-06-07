package alpine

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

func (p *Provider) Type() models.PackageType { return models.PackageAlpine }

func (p *Provider) Classify(path string) provider.Mutability {
	if strings.HasSuffix(path, "APKINDEX.tar.gz") {
		return provider.Mutable
	}
	return provider.Immutable
}

func (p *Provider) ContentType(path string) string {
	if strings.HasSuffix(path, ".apk") {
		return "application/vnd.android.package-archive"
	}
	if strings.HasSuffix(path, ".tar.gz") {
		return "application/gzip"
	}
	return "application/octet-stream"
}

func (p *Provider) UpstreamURL(remote models.Remote, path string) string {
	return strings.TrimRight(remote.BaseURL, "/") + "/" + strings.TrimLeft(path, "/")
}

func (p *Provider) RewriteResponse(_ []byte, _ models.Remote, _ string) ([]byte, error) {
	return nil, nil
}

func (p *Provider) AuthHeaders(_ context.Context, remote models.Remote) (http.Header, error) {
	return auth.BasicHeaders(remote), nil
}
