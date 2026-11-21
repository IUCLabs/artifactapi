package terraform_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/IUCLabs/artifactapi/internal/provider"
	"github.com/IUCLabs/artifactapi/internal/provider/terraform"
	"github.com/IUCLabs/artifactapi/pkg/models"
)

func TestProvider_Type(t *testing.T) {
	p := &terraform.Provider{}
	if p.Type() != models.PackageTerraform {
		t.Errorf("expected terraform, got %q", p.Type())
	}
}

func TestProvider_Classify(t *testing.T) {
	p := &terraform.Provider{}
	tests := []struct {
		path string
		want provider.Mutability
	}{
		{"hashicorp/vault/versions", provider.Mutable},
		{"hashicorp/vault/0.28.0/download/linux/amd64", provider.Immutable},
	}
	for _, tt := range tests {
		if got := p.Classify(tt.path); got != tt.want {
			t.Errorf("Classify(%q) = %v, want %v", tt.path, got, tt.want)
		}
	}
}

func TestProvider_RewriteResponse_DownloadInfo(t *testing.T) {
	p := &terraform.Provider{}
	remote := models.Remote{Name: "tf", ReleasesRemote: "hashicorp-releases"}
	body, _ := json.Marshal(map[string]any{
		"download_url": "https://releases.hashicorp.com/terraform-provider-vault/0.28.0/file.zip",
		"shasums_url":  "https://releases.hashicorp.com/terraform-provider-vault/0.28.0/SHA256SUMS",
	})
	rewritten, err := p.RewriteResponse(body, remote, "https://proxy")
	if err != nil {
		t.Fatal(err)
	}
	if rewritten == nil {
		t.Fatal("expected rewrite")
	}
	var result map[string]any
	if err := json.Unmarshal(rewritten, &result); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result["download_url"].(string), "proxy/api/v1/remote/hashicorp-releases") {
		t.Errorf("download_url not rewritten: %s", result["download_url"])
	}
}
