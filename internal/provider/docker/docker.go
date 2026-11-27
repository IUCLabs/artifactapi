package docker

import (
	"context"
	"encoding/base64"
	"net/http"
	"regexp"
	"strings"

	"github.com/IUCLabs/artifactapi/internal/provider"
	"github.com/IUCLabs/artifactapi/pkg/models"
)

func init() {
	provider.Register(&Provider{})
}

var (
	tagManifestRe    = regexp.MustCompile(`/manifests/[^/]+$`)
	digestManifestRe = regexp.MustCompile(`/manifests/sha256:[0-9a-fA-F]+$`)
	tagsListRe       = regexp.MustCompile(`/tags/list$`)
)

type Provider struct{}

func (p *Provider) Type() models.PackageType { return models.PackageDocker }

func (p *Provider) Classify(path string) provider.Mutability {
	if tagsListRe.MatchString(path) {
		return provider.Mutable
	}
	if tagManifestRe.MatchString(path) && !digestManifestRe.MatchString(path) {
		return provider.Mutable
	}
	return provider.Immutable
}

func (p *Provider) ContentType(path string) string {
	if strings.Contains(path, "/blobs/") {
		return "application/octet-stream"
	}
	if strings.Contains(path, "/manifests/") {
		return "application/vnd.docker.distribution.manifest.v2+json"
	}
	return "application/json"
}

func (p *Provider) UpstreamURL(remote models.Remote, path string) string {
	return strings.TrimRight(remote.BaseURL, "/") + "/v2/" + strings.TrimLeft(path, "/")
}

func (p *Provider) AuthHeaders(_ context.Context, remote models.Remote) (http.Header, error) {
	h := http.Header{}
	if remote.Username != "" && remote.Password != "" {
		h.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(remote.Username+":"+remote.Password)))
	}
	return h, nil
}
