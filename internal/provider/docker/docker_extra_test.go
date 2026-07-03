package docker

import (
	"context"
	"testing"

	"github.com/IUCLabs/artifactapi/internal/provider"
	"github.com/IUCLabs/artifactapi/pkg/models"
)

func TestDockerClassifyBranches(t *testing.T) {
	p := &Provider{}
	if p.Classify("library/nginx/tags/list") != provider.Mutable {
		t.Error("tags/list should be mutable")
	}
	if p.Classify("library/nginx/manifests/latest") != provider.Mutable {
		t.Error("tag manifest should be mutable")
	}
	if p.Classify("library/nginx/manifests/sha256:abcdef") != provider.Immutable {
		t.Error("digest manifest should be immutable")
	}
	if p.Classify("library/nginx/blobs/sha256:abc") != provider.Immutable {
		t.Error("blob should be immutable")
	}
}

func TestDockerContentType(t *testing.T) {
	p := &Provider{}
	if p.ContentType("x/blobs/sha256:abc") != "application/octet-stream" {
		t.Error("blob content type")
	}
	if p.ContentType("x/manifests/latest") != "application/vnd.docker.distribution.manifest.v2+json" {
		t.Error("manifest content type")
	}
	if p.ContentType("x/tags/list") != "application/json" {
		t.Error("default content type")
	}
}

func TestDockerRewriteAndAuth(t *testing.T) {
	p := &Provider{}
	if out, err := p.RewriteResponse([]byte("x"), models.Remote{}, "http://p"); out != nil || err != nil {
		t.Error("docker never rewrites")
	}
	h, _ := p.AuthHeaders(context.Background(), models.Remote{Username: "u", Password: "p"})
	if h.Get("Authorization") == "" {
		t.Error("expected basic auth header")
	}
	h, _ = p.AuthHeaders(context.Background(), models.Remote{})
	if h.Get("Authorization") != "" {
		t.Error("no creds, no header")
	}
}
