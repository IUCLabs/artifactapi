package puppet

import (
	"context"
	"net/http"
	"strconv"
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

// ForwardedQuery keeps the module filter and pagination on release listings.
func (p *Provider) ForwardedQuery(path string) map[string]provider.QueryValidator {
	if path != "v3/releases" {
		return nil
	}
	return map[string]provider.QueryValidator{"module": moduleParam, "limit": pageLimit, "offset": pageOffset}
}

func moduleParam(v string) (string, bool) {
	v = NormalizeSlug(v)
	return v, ValidSlug(v)
}

func pageLimit(v string) (string, bool) {
	n, err := strconv.ParseUint(v, 10, 32)
	if err != nil || n == 0 {
		return "", false
	}
	return strconv.FormatUint(min(n, 100), 10), true
}

func pageOffset(v string) (string, bool) {
	n, err := strconv.ParseUint(v, 10, 32)
	return strconv.FormatUint(n, 10), err == nil
}

func (p *Provider) AuthHeaders(_ context.Context, remote models.Remote) (http.Header, error) {
	return auth.BasicHeaders(remote), nil
}
