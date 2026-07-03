package npm

import (
	"context"
	"testing"

	"github.com/IUCLabs/artifactapi/internal/provider"
	"github.com/IUCLabs/artifactapi/pkg/models"
)

func TestType(t *testing.T) {
	if (&Provider{}).Type() != models.PackageNPM {
		t.Fatal("wrong type")
	}
}

func TestClassify(t *testing.T) {
	p := &Provider{}
	if p.Classify("pkg/-/pkg-1.0.0.tgz") != provider.Immutable {
		t.Error("tgz should be immutable")
	}
	if p.Classify("pkg") != provider.Mutable {
		t.Error("metadata should be mutable")
	}
}

func TestContentType(t *testing.T) {
	p := &Provider{}
	if p.ContentType("pkg/-/pkg-1.0.0.tgz") != "application/gzip" {
		t.Error("tgz content type")
	}
	if p.ContentType("pkg") != "application/json" {
		t.Error("metadata content type")
	}
}

func TestUpstreamURL(t *testing.T) {
	p := &Provider{}
	got := p.UpstreamURL(models.Remote{BaseURL: "https://registry.npmjs.org/"}, "/pkg")
	if got != "https://registry.npmjs.org/pkg" {
		t.Errorf("got %q", got)
	}
}

func TestRewriteResponse(t *testing.T) {
	p := &Provider{}
	remote := models.Remote{Name: "npmjs", BaseURL: "https://registry.npmjs.org"}

	if out, _ := p.RewriteResponse([]byte(`{"a":1}`), remote, ""); out != nil {
		t.Error("empty proxyBaseURL should be a no-op")
	}
	if out, _ := p.RewriteResponse([]byte("not json"), remote, "http://proxy"); out != nil {
		t.Error("invalid json should be a no-op")
	}
	body := []byte(`{"tarball":"https://registry.npmjs.org/pkg/-/pkg-1.0.0.tgz"}`)
	out, err := p.RewriteResponse(body, remote, "http://proxy")
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != `{"tarball":"http://proxy/api/v1/remote/npmjs/pkg/-/pkg-1.0.0.tgz"}` {
		t.Errorf("rewrite: %s", out)
	}
	if out, _ := p.RewriteResponse([]byte(`{"x":"unrelated"}`), remote, "http://proxy"); out != nil {
		t.Error("no matching base URL should be a no-op")
	}
}

func TestAuthHeaders(t *testing.T) {
	p := &Provider{}
	h, _ := p.AuthHeaders(context.Background(), models.Remote{Username: "u", Password: "pw"})
	if h.Get("Authorization") == "" {
		t.Error("expected auth header when credentials set")
	}
	h, _ = p.AuthHeaders(context.Background(), models.Remote{})
	if h.Get("Authorization") != "" {
		t.Error("expected no auth header without credentials")
	}
}
