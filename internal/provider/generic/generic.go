package generic

import (
	"context"
	"encoding/base64"
	"net/http"
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

// AuthHeaders authenticates outbound requests. A remote github_app (GitHub
// hosts only) wins, then a per-remote username/password (Basic auth), then the
// process-wide GitHub credential for GitHub hosts so private downloads work.
func (p *Provider) AuthHeaders(ctx context.Context, remote models.Remote) (http.Header, error) {
	h := http.Header{}
	github := IsGitHubHost(remote.BaseURL)
	if remote.GitHubApp != "" && github {
		tok, err := githubauth.AppToken(ctx, githubauth.AppRegistry(), remote.GitHubApp)
		if err != nil {
			return nil, err
		}
		h.Set("Authorization", "Bearer "+tok)
		return h, nil
	}
	if remote.Username != "" {
		h.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(remote.Username+":"+remote.Password)))
		return h, nil
	}
	if github {
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

// IsGitHubHost reports whether rawURL targets a GitHub host that accepts a
// GitHub credential; see models.IsGitHubHost.
func IsGitHubHost(rawURL string) bool { return models.IsGitHubHost(rawURL) }
