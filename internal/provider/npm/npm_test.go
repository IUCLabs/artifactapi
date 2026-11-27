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

func TestRewriteServed(t *testing.T) {
	p := &Provider{}
	remote := models.Remote{Name: "npmjs", BaseURL: "https://registry.npmjs.org/"}
	const tarball = "https://registry.npmjs.org/@scope/pkg/-/pkg-1.0.0.tgz"

	out := string(p.RewriteServed([]byte(`{"dist":{"tarball":"`+tarball+`"},"homepage":"https://registry.npmjs.org.evil/x"}`), remote, "https://aa.example:8443/"))
	want := `{"dist":{"tarball":"https://aa.example:8443/api/v1/remote/npmjs/@scope/pkg/-/pkg-1.0.0.tgz"},"homepage":"https://registry.npmjs.org.evil/x"}`
	if out != want {
		t.Fatalf("got %s, want %s", out, want)
	}
	path := "@scope/pkg/-/pkg-1.0.0.tgz"
	if p.Classify(path) != provider.Immutable || p.UpstreamURL(remote, path) != tarball {
		t.Errorf("rewritten tarball %q must route back to %q as an immutable artifact", path, tarball)
	}
	if got := p.RewriteServed([]byte("not json"), remote, "https://aa.example"); string(got) != "not json" {
		t.Errorf("non-JSON body changed: %s", got)
	}
	if _, ok := any(p).(provider.ResponseRewriter); ok {
		t.Error("npm rewrites at serve time, not before caching")
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
