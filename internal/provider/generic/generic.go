package generic

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/url"
	"path"
	"strings"

	"github.com/IUCLabs/artifactapi/internal/githubauth"
	"github.com/IUCLabs/artifactapi/internal/provider"
	"github.com/IUCLabs/artifactapi/pkg/models"
)

func init() {
	provider.Register(&Provider{})
}

type Provider struct{}

func (p *Provider) Type() models.PackageType { return models.PackageGeneric }

func (p *Provider) Classify(_ string) provider.Mutability {
	return provider.Immutable
}

var contentTypeMap = map[string]string{
	".tar.gz": "application/gzip",
	".tgz":    "application/gzip",
	".gz":     "application/gzip",
	".zip":    "application/zip",
	".whl":    "application/zip",
	".exe":    "application/x-msdownload",
	".rpm":    "application/x-rpm",
	".xml":    "application/xml",
	".yaml":   "text/yaml",
	".yml":    "text/yaml",
	".json":   "application/json",
	".sig":    "application/octet-stream",
}

func (p *Provider) ContentType(filePath string) string {
	lower := strings.ToLower(filePath)
	if strings.HasSuffix(lower, ".tar.gz") {
		return "application/gzip"
	}
	ext := path.Ext(lower)
	if ct, ok := contentTypeMap[ext]; ok {
		return ct
	}
	return "application/octet-stream"
}

func (p *Provider) UpstreamURL(remote models.Remote, reqPath string) string {
	base := strings.TrimRight(remote.BaseURL, "/")
	return base + "/" + strings.TrimLeft(reqPath, "/")
}

func (p *Provider) RewriteResponse(_ []byte, _ models.Remote, _ string) ([]byte, error) {
	return nil, nil
}

// AuthHeaders authenticates outbound requests. A per-remote username/password
// (Basic auth) takes precedence. Otherwise, when the remote points at a GitHub
// host (e.g. a releases_remote proxying private release assets), the process-wide
// GitHub credential is attached as a bearer token so private downloads work.
func (p *Provider) AuthHeaders(ctx context.Context, remote models.Remote) (http.Header, error) {
	h := http.Header{}
	if remote.Username != "" {
		h.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(remote.Username+":"+remote.Password)))
		return h, nil
	}
	if isGitHubHost(remote.BaseURL) {
		if c := githubauth.Server(); c != nil {
			tok, err := c.Token(ctx)
			if err != nil {
				return nil, err
			}
			if tok != "" {
				h.Set("Authorization", "Bearer "+tok)
			}
		}
	}
	return h, nil
}

// isGitHubHost reports whether rawURL targets a GitHub API/download host that
// accepts the server credential. objects.githubusercontent.com is deliberately
// excluded: release-asset downloads 302-redirect there with a pre-signed URL
// that must not carry an Authorization header.
func isGitHubHost(rawURL string) bool {
	u, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	switch strings.ToLower(u.Hostname()) {
	case "github.com", "www.github.com", "api.github.com", "codeload.github.com", "uploads.github.com":
		return true
	}
	return false
}
