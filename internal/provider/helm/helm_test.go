package helm_test

import (
	"strings"
	"testing"

	"github.com/IUCLabs/artifactapi/internal/provider"
	"github.com/IUCLabs/artifactapi/internal/provider/helm"
	"github.com/IUCLabs/artifactapi/pkg/models"
)

func TestProvider_Type(t *testing.T) {
	p := &helm.Provider{}
	if p.Type() != models.PackageHelm {
		t.Errorf("expected helm, got %q", p.Type())
	}
}

func TestProvider_Classify(t *testing.T) {
	p := &helm.Provider{}
	tests := []struct {
		path string
		want provider.Mutability
	}{
		{"index.yaml", provider.Mutable},
		{"index.yml", provider.Mutable},
		{"chart-1.0.tgz", provider.Immutable},
		{"charts/nginx-1.0.tgz", provider.Immutable},
	}
	for _, tt := range tests {
		if got := p.Classify(tt.path); got != tt.want {
			t.Errorf("Classify(%q) = %v, want %v", tt.path, got, tt.want)
		}
	}
}

func TestProvider_RewriteServed(t *testing.T) {
	p := &helm.Provider{}
	remote := models.Remote{Name: "helm-test", BaseURL: "https://charts.example.com/"}
	body := []byte("entries:\n  a:\n  - urls:\n    - https://charts.example.com/charts/a-1.0.tgz\n  b:\n  - urls:\n    - b-2.0.tgz\n    - https://github.com/o/r/releases/download/b-2.0.tgz\n")

	out := string(p.RewriteServed(body, remote, "https://proxy.example.com"))
	if !strings.Contains(out, "- https://proxy.example.com/api/v1/remote/helm-test/charts/a-1.0.tgz\n") {
		t.Errorf("absolute chart url not rewritten: %s", out)
	}
	if !strings.Contains(out, "- b-2.0.tgz\n") || !strings.Contains(out, "https://github.com/o/r/releases/download/b-2.0.tgz") {
		t.Errorf("relative or foreign urls changed: %s", out)
	}
	if p.Classify("charts/a-1.0.tgz") != provider.Immutable || p.UpstreamURL(remote, "charts/a-1.0.tgz") != "https://charts.example.com/charts/a-1.0.tgz" {
		t.Error("rewritten chart must route back to the upstream chart as an immutable artifact")
	}
	if _, ok := any(p).(provider.ResponseRewriter); ok {
		t.Error("helm rewrites at serve time, not before caching")
	}
}
