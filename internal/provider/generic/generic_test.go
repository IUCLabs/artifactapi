package generic_test

import (
	"context"
	"testing"

	"github.com/IUCLabs/artifactapi/internal/provider"
	"github.com/IUCLabs/artifactapi/internal/provider/generic"
	"github.com/IUCLabs/artifactapi/pkg/models"
)

func TestProvider_Type(t *testing.T) {
	p := &generic.Provider{}
	if p.Type() != models.PackageGeneric {
		t.Errorf("expected generic, got %q", p.Type())
	}
}

func TestProvider_Classify_AllImmutable(t *testing.T) {
	p := &generic.Provider{}
	paths := []string{"file.tar.gz", "path/to/binary", "index.html", "data.json"}
	for _, path := range paths {
		if p.Classify(path) != provider.Immutable {
			t.Errorf("generic should classify %q as immutable", path)
		}
	}
}

func TestProvider_ContentType(t *testing.T) {
	p := &generic.Provider{}
	tests := []struct{ path, want string }{
		{"file.tar.gz", "application/gzip"},
		{"file.tgz", "application/gzip"},
		{"file.zip", "application/zip"},
		{"file.rpm", "application/x-rpm"},
		{"file.json", "application/json"},
		{"file.unknown", "application/octet-stream"},
	}
	for _, tt := range tests {
		if got := p.ContentType(tt.path); got != tt.want {
			t.Errorf("ContentType(%q) = %q, want %q", tt.path, got, tt.want)
		}
	}
}

func TestProvider_UpstreamURL(t *testing.T) {
	p := &generic.Provider{}
	got := p.UpstreamURL(models.Remote{BaseURL: "https://example.com/repo"}, "path/to/file.tar.gz")
	want := "https://example.com/repo/path/to/file.tar.gz"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestProvider_AuthHeaders_BasicAuth(t *testing.T) {
	p := &generic.Provider{}
	h, _ := p.AuthHeaders(context.Background(), models.Remote{Username: "user", Password: "pass"})
	if h.Get("Authorization") != "Basic dXNlcjpwYXNz" {
		t.Errorf("unexpected auth header: %q", h.Get("Authorization"))
	}
}

func TestProvider_AuthHeaders_NoAuth(t *testing.T) {
	p := &generic.Provider{}
	h, _ := p.AuthHeaders(context.Background(), models.Remote{})
	if h.Get("Authorization") != "" {
		t.Error("expected no auth header")
	}
}
