package rpm

import (
	"context"
	"net/http"
	"regexp"
	"strings"

	"github.com/IUCLabs/artifactapi/internal/auth"
	"github.com/IUCLabs/artifactapi/internal/provider"
	"github.com/IUCLabs/artifactapi/pkg/models"
)

func init() {
	provider.Register(&Provider{})
}

var mutableRe = []*regexp.Regexp{
	regexp.MustCompile(`repomd\.xml$`),
	regexp.MustCompile(`repodata/`),
	regexp.MustCompile(`Packages\.gz$`),
}

type Provider struct{}

func (p *Provider) Type() models.PackageType { return models.PackageRPM }

func (p *Provider) Classify(path string) provider.Mutability {
	for _, re := range mutableRe {
		if re.MatchString(path) {
			return provider.Mutable
		}
	}
	return provider.Immutable
}

func (p *Provider) ContentType(path string) string {
	if strings.HasSuffix(path, ".rpm") {
		return "application/x-rpm"
	}
	if strings.HasSuffix(path, ".xml") || strings.HasSuffix(path, ".xml.gz") || strings.HasSuffix(path, ".xml.xz") {
		return "application/xml"
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
