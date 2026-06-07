package goproxy_test

import (
	"testing"

	"github.com/IUCLabs/artifactapi/internal/provider"
	"github.com/IUCLabs/artifactapi/internal/provider/goproxy"
	"github.com/IUCLabs/artifactapi/pkg/models"
)

func TestProvider_Type(t *testing.T) {
	p := &goproxy.Provider{}
	if p.Type() != models.PackageGoProxy {
		t.Errorf("expected goproxy, got %q", p.Type())
	}
}

func TestProvider_Classify(t *testing.T) {
	p := &goproxy.Provider{}
	tests := []struct {
		path string
		want provider.Mutability
	}{
		{"golang.org/x/net/@v/list", provider.Mutable},
		{"golang.org/x/net/@latest", provider.Mutable},
		{"golang.org/x/net/@v/v0.1.0.info", provider.Immutable},
		{"golang.org/x/net/@v/v0.1.0.mod", provider.Immutable},
		{"golang.org/x/net/@v/v0.1.0.zip", provider.Immutable},
	}
	for _, tt := range tests {
		if got := p.Classify(tt.path); got != tt.want {
			t.Errorf("Classify(%q) = %v, want %v", tt.path, got, tt.want)
		}
	}
}

func TestProvider_ContentType(t *testing.T) {
	p := &goproxy.Provider{}
	tests := []struct{ path, want string }{
		{"m/@v/v1.0.0.zip", "application/zip"},
		{"m/@v/v1.0.0.mod", "text/plain"},
		{"m/@v/v1.0.0.info", "application/json"},
		{"m/@v/list", "text/plain"},
	}
	for _, tt := range tests {
		if got := p.ContentType(tt.path); got != tt.want {
			t.Errorf("ContentType(%q) = %q, want %q", tt.path, got, tt.want)
		}
	}
}
