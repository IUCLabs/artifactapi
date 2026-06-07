package generic

import (
	"context"
	"encoding/base64"
	"net/http"
	"path"
	"strings"

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

func (p *Provider) AuthHeaders(_ context.Context, remote models.Remote) (http.Header, error) {
	h := http.Header{}
	if remote.Username != "" {
		h.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(remote.Username+":"+remote.Password)))
	}
	return h, nil
}
