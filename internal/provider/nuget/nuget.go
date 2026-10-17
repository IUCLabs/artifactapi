// Package nuget proxies a NuGet v3 feed: the service index, registration pages
// and flat container indexes are mutable, .nupkg/.nuspec downloads immutable.
package nuget

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"

	"github.com/IUCLabs/artifactapi/internal/auth"
	"github.com/IUCLabs/artifactapi/internal/provider"
	"github.com/IUCLabs/artifactapi/pkg/models"
)

func init() {
	provider.Register(&Provider{})
}

// serviceIndex is the path clients use as their source URL; it maps to base_url.
const serviceIndex = "index.json"

type Provider struct{}

func (p *Provider) Type() models.PackageType { return models.PackageNuGet }

func (p *Provider) Classify(path string) provider.Mutability {
	if isPackage(path) {
		return provider.Immutable
	}
	return provider.Mutable
}

func (p *Provider) ContentType(path string) string {
	switch {
	case strings.HasSuffix(path, ".nupkg"):
		return "application/octet-stream"
	case strings.HasSuffix(path, ".nuspec"):
		return "application/xml"
	}
	return "application/json"
}

// UpstreamURL maps index.json to base_url (the upstream service index) and any
// other path to the same path on the base_url's origin, where nuget.org serves
// v3-flatcontainer/ and v3/registration*/.
func (p *Provider) UpstreamURL(remote models.Remote, path string) string {
	path = strings.TrimLeft(path, "/")
	if path == serviceIndex {
		return remote.BaseURL
	}
	return origin(remote.BaseURL) + "/" + path
}

func (p *Provider) RewriteResponse(_ []byte, _ models.Remote, _ string) ([]byte, error) {
	return nil, nil
}

// RewriteServed points upstream-origin URLs in cached JSON back at this remote.
// It runs per request because the URLs must name the requesting host.
func (p *Provider) RewriteServed(body []byte, remote models.Remote, proxyBaseURL string) []byte {
	from := origin(remote.BaseURL)
	if from == "" || !json.Valid(body) {
		return body
	}
	to := strings.TrimRight(proxyBaseURL, "/") + "/api/v1/remote/" + remote.Name
	return []byte(strings.ReplaceAll(string(body), from, to))
}

func (p *Provider) AuthHeaders(_ context.Context, remote models.Remote) (http.Header, error) {
	return auth.BasicHeaders(remote), nil
}

func origin(baseURL string) string {
	u, err := url.Parse(baseURL)
	if err != nil || u.Host == "" {
		return ""
	}
	return u.Scheme + "://" + u.Host
}

func isPackage(path string) bool {
	return strings.HasSuffix(path, ".nupkg") || strings.HasSuffix(path, ".nuspec")
}
