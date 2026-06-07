package provider

import (
	"context"
	"fmt"
	"net/http"

	"github.com/IUCLabs/artifactapi/pkg/models"
)

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
	RewriteResponse(body []byte, remote models.Remote, proxyBaseURL string) ([]byte, error)
	AuthHeaders(ctx context.Context, remote models.Remote) (http.Header, error)
}

type IndexMerger interface {
	MergeIndexes(members []MemberIndex, proxyBaseURL string) ([]byte, error)
}

type MemberIndex struct {
	RemoteName string
	Body       []byte
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
