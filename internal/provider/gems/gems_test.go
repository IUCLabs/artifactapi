package gems

import (
	"testing"

	"github.com/IUCLabs/artifactapi/internal/provider"
	"github.com/IUCLabs/artifactapi/pkg/models"
)

func TestClassify(t *testing.T) {
	p := &Provider{}
	tests := []struct {
		path string
		want provider.Mutability
	}{
		{"gems/rack-3.1.8.gem", provider.Immutable},
		{"/gems/nokogiri-1.16.7-x86_64-linux.gem", provider.Immutable},
		{"quick/Marshal.4.8/rack-3.1.8.gemspec.rz", provider.Immutable},
		{"versions", provider.Mutable},
		{"names", provider.Mutable},
		{"info/rack", provider.Mutable},
		{"info/gems", provider.Mutable},
		{"specs.4.8.gz", provider.Mutable},
		{"latest_specs.4.8.gz", provider.Mutable},
		{"prerelease_specs.4.8.gz", provider.Mutable},
		{"api/v1/dependencies", provider.Mutable},
		{"quick/Marshal.4.8/", provider.Mutable},
	}
	for _, tt := range tests {
		if got := p.Classify(tt.path); got != tt.want {
			t.Errorf("Classify(%q) = %v, want %v", tt.path, got, tt.want)
		}
	}
}

func TestContentType(t *testing.T) {
	p := &Provider{}
	tests := map[string]string{
		"versions":                 "text/plain; charset=utf-8",
		"info/rack":                "text/plain; charset=utf-8",
		"names":                    "text/plain; charset=utf-8",
		"gems/rack-3.1.8.gem":      "application/octet-stream",
		"specs.4.8.gz":             "application/octet-stream",
		"api/v1/gems/rack.json":    "application/json",
		"api/v1/dependencies.json": "application/json",
		"api/v1/dependencies":      "application/octet-stream",
	}
	for path, want := range tests {
		if got := p.ContentType(path); got != want {
			t.Errorf("ContentType(%q) = %q, want %q", path, got, want)
		}
	}
}

func TestUpstreamURL(t *testing.T) {
	p := &Provider{}
	r := models.Remote{BaseURL: "https://rubygems.org/"}
	if got := p.UpstreamURL(r, "/info/rack"); got != "https://rubygems.org/info/rack" {
		t.Errorf("UpstreamURL = %q", got)
	}
}
