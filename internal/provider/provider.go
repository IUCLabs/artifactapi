package provider

import (
	"context"
	"fmt"
	"net/http"

	"github.com/IUCLabs/artifactapi/pkg/models"
)

// RemoteMountPath is the URL path remote proxies are served under.
const RemoteMountPath = "/api/v1/remote/"

type Mutability int

const (
	Immutable Mutability = iota
	Mutable
)

type Provider interface {
	Type() models.PackageType
	Classify(path string) Mutability
	ContentType(path string) string
	UpstreamURL(remote models.Remote, path string) string
	AuthHeaders(ctx context.Context, remote models.Remote) (http.Header, error)
}

type FileEntry struct {
	FilePath    string
	ContentHash string
}

var registry = map[models.PackageType]Provider{}

func Register(p Provider) {
	registry[p.Type()] = p
}

func Get(t models.PackageType) (Provider, error) {
	p, ok := registry[t]
	if !ok {
		return nil, fmt.Errorf("no provider registered for package type %q", t)
	}
	return p, nil
}

func All() map[models.PackageType]Provider {
	return registry
}
