// Package cargo proxies a Cargo sparse registry (RFC 2789): mutable index
// files plus immutable .crate downloads, with config.json synthesized so
// cargo fetches crates back through artifactapi.
package cargo

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

const (
	crateDir         = "crates/"
	cratesIOIndex    = "index.crates.io"
	cratesIODownload = "https://static.crates.io"
)

type Provider struct{}

func (p *Provider) Type() models.PackageType { return models.PackageCargo }

func (p *Provider) Classify(path string) provider.Mutability {
	if isCrate(path) {
		return provider.Immutable
	}
	return provider.Mutable
}

func (p *Provider) ContentType(path string) string {
	if isCrate(path) {
		return "application/gzip"
	}
	if path == "config.json" {
		return "application/json"
	}
	return "text/plain"
}

// UpstreamURL maps crates/{crate}/{crate}-{version}.crate to the download host
// and everything else to the index. crates.io splits the two across hosts;
// any other upstream is expected to serve both.
func (p *Provider) UpstreamURL(remote models.Remote, path string) string {
	path = strings.TrimLeft(path, "/")
	base := strings.TrimRight(remote.BaseURL, "/")
	if isCrate(path) {
		if u, err := url.Parse(base); err == nil && u.Host == cratesIOIndex {
			base = cratesIODownload
		}
	}
	return base + "/" + path
}

func (p *Provider) AuthHeaders(_ context.Context, remote models.Remote) (http.Header, error) {
	return auth.BasicHeaders(remote), nil
}

// ServeRemote answers config.json itself: dl must name the requesting host,
// which a cached upstream body cannot.
func (p *Provider) ServeRemote(w http.ResponseWriter, r *http.Request, remote models.Remote, path, proxyBaseURL string, _ provider.RemoteMetadataStore) bool {
	if strings.TrimLeft(path, "/") != "config.json" {
		return false
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(Config(remote, proxyBaseURL))
	return true
}

// Config builds the sparse registry config.json pointing downloads at proxyBaseURL.
func Config(remote models.Remote, proxyBaseURL string) map[string]string {
	return map[string]string{
		"dl": strings.TrimRight(proxyBaseURL, "/") + "/api/v1/remote/" + remote.Name + "/" + crateDir + "{crate}/{crate}-{version}.crate",
	}
}

func isCrate(path string) bool {
	path = strings.TrimLeft(path, "/")
	return strings.HasPrefix(path, crateDir) && strings.HasSuffix(path, ".crate")
}
