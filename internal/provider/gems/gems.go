// Package gems proxies a RubyGems source: mutable compact index (versions,
// info/*, names) and legacy specs indexes, plus immutable .gem and
// quick/Marshal.4.8 gemspec downloads.
package gems

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

func (p *Provider) Type() models.PackageType { return models.PackageGems }

func (p *Provider) Classify(path string) provider.Mutability {
	if isGem(path) || isQuickSpec(path) {
		return provider.Immutable
	}
	return provider.Mutable
}

func (p *Provider) ContentType(path string) string {
	path = strings.TrimLeft(path, "/")
	if path == "versions" || path == "names" || strings.HasPrefix(path, "info/") {
		return "text/plain; charset=utf-8"
	}
	if strings.HasPrefix(path, "api/v1/") && strings.HasSuffix(path, ".json") {
		return "application/json"
	}
	return "application/octet-stream"
}

func (p *Provider) UpstreamURL(remote models.Remote, path string) string {
	return strings.TrimRight(remote.BaseURL, "/") + "/" + strings.TrimLeft(path, "/")
}

func (p *Provider) AuthHeaders(_ context.Context, remote models.Remote) (http.Header, error) {
	return auth.BasicHeaders(remote), nil
}

func isGem(path string) bool {
	path = strings.TrimLeft(path, "/")
	return strings.HasPrefix(path, "gems/") && strings.HasSuffix(path, ".gem")
}

func isQuickSpec(path string) bool {
	path = strings.TrimLeft(path, "/")
	return strings.HasPrefix(path, "quick/Marshal.4.8/") && strings.HasSuffix(path, ".gemspec.rz")
}
