package terraform

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"regexp"
	"strings"

	"github.com/IUCLabs/artifactapi/internal/auth"
	"github.com/IUCLabs/artifactapi/internal/provider"
	"github.com/IUCLabs/artifactapi/pkg/models"
)

func init() {
	provider.Register(&Provider{})
}

var versionsRe = regexp.MustCompile(`[^/]+/[^/]+/versions$`)

type Provider struct{}

func (p *Provider) Type() models.PackageType { return models.PackageTerraform }

func (p *Provider) Classify(path string) provider.Mutability {
	if versionsRe.MatchString(path) {
		return provider.Mutable
	}
	return provider.Immutable
}

func (p *Provider) ContentType(path string) string {
	lower := strings.ToLower(path)
	if strings.HasSuffix(lower, ".zip") {
		return "application/zip"
	}
	if strings.HasSuffix(lower, ".sig") {
		return "application/octet-stream"
	}
	return "application/json"
}

func (p *Provider) UpstreamURL(remote models.Remote, path string) string {
	return strings.TrimRight(remote.BaseURL, "/") + "/v1/providers/" + strings.TrimLeft(path, "/")
}

func (p *Provider) RewriteResponse(body []byte, remote models.Remote, proxyBaseURL string) ([]byte, error) {
	if remote.ReleasesRemote == "" {
		return nil, nil
	}
	if !json.Valid(body) {
		return nil, nil
	}

	var data map[string]any
	if err := json.Unmarshal(body, &data); err != nil {
		return nil, nil
	}

	changed := false
	for _, field := range []string{"download_url", "shasums_url", "shasums_signature_url"} {
		if val, ok := data[field].(string); ok && val != "" {
			rewritten := rewriteDownloadURL(val, remote.ReleasesRemote, proxyBaseURL)
			if rewritten != val {
				data[field] = rewritten
				changed = true
			}
		}
	}

	if !changed {
		return nil, nil
	}
	return json.Marshal(data)
}

func rewriteDownloadURL(originalURL, releasesRemote, proxyBaseURL string) string {
	parsed, err := url.Parse(originalURL)
	if err != nil || proxyBaseURL == "" {
		return originalURL
	}
	return strings.TrimRight(proxyBaseURL, "/") + "/api/v1/remote/" + releasesRemote + parsed.Path
}

func (p *Provider) AuthHeaders(_ context.Context, remote models.Remote) (http.Header, error) {
	return auth.BasicHeaders(remote), nil
}
