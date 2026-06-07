package docker_test

import (
	"testing"

	"github.com/IUCLabs/artifactapi/internal/provider"
	"github.com/IUCLabs/artifactapi/internal/provider/docker"
	"github.com/IUCLabs/artifactapi/pkg/models"
)

func TestProvider_Type(t *testing.T) {
	p := &docker.Provider{}
	if p.Type() != models.PackageDocker {
		t.Errorf("expected docker, got %q", p.Type())
	}
}

func TestProvider_Classify(t *testing.T) {
	p := &docker.Provider{}
	tests := []struct {
		path string
		want provider.Mutability
	}{
		{"library/nginx/manifests/latest", provider.Mutable},
		{"library/nginx/manifests/v1.25", provider.Mutable},
		{"library/nginx/manifests/sha256:abcdef1234567890abcdef1234567890abcdef1234567890abcdef1234567890", provider.Immutable},
		{"library/nginx/tags/list", provider.Mutable},
		{"library/nginx/blobs/sha256:abc123", provider.Immutable},
	}
	for _, tt := range tests {
		if got := p.Classify(tt.path); got != tt.want {
			t.Errorf("Classify(%q) = %v, want %v", tt.path, got, tt.want)
		}
	}
}

func TestProvider_UpstreamURL(t *testing.T) {
	p := &docker.Provider{}
	got := p.UpstreamURL(models.Remote{BaseURL: "https://registry-1.docker.io"}, "library/nginx/manifests/latest")
	want := "https://registry-1.docker.io/v2/library/nginx/manifests/latest"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestProvider_ContentType(t *testing.T) {
	p := &docker.Provider{}
	if p.ContentType("x/blobs/sha256:abc") != "application/octet-stream" {
		t.Error("blobs should be octet-stream")
	}
	if p.ContentType("x/manifests/latest") != "application/vnd.docker.distribution.manifest.v2+json" {
		t.Error("manifests should be manifest type")
	}
}
