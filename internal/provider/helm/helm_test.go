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

func TestProvider_RewriteResponse(t *testing.T) {
	p := &helm.Provider{}
	body := []byte("urls:\n- https://charts.example.com/chart-1.0.tgz")
	remote := models.Remote{Name: "helm-test", BaseURL: "https://charts.example.com"}
	rewritten, err := p.RewriteResponse(body, remote, "https://proxy.example.com")
	if err != nil {
		t.Fatal(err)
	}
	if rewritten == nil {
		t.Fatal("expected rewrite")
	}
	if !strings.Contains(string(rewritten), "proxy.example.com/api/v1/remote/helm-test") {
		t.Errorf("expected proxy URL in body: %s", rewritten)
	}
}
