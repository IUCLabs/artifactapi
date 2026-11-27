package terraform_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/IUCLabs/artifactapi/internal/provider"
	"github.com/IUCLabs/artifactapi/internal/provider/generic"
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
		{"hashicorp/vault/0.28.0/download/linux/amd64", provider.Mutable},
		{"hashicorp/aws/download/pkg.zip", provider.Immutable},
	}
	for _, tt := range tests {
		if got := p.Classify(tt.path); got != tt.want {
			t.Errorf("Classify(%q) = %v, want %v", tt.path, got, tt.want)
		}
	}
}

func TestProvider_RewriteServed_DownloadDoc(t *testing.T) {
	p := &terraform.Provider{}
	remote := models.Remote{Name: "tf", ReleasesRemote: "hashicorp-releases"}
	releases := models.Remote{Name: "hashicorp-releases", BaseURL: "https://releases.hashicorp.com"}
	const ver = "https://releases.hashicorp.com/terraform-provider-null/3.2.4/"
	body, _ := json.Marshal(map[string]any{
		"os":                    "linux",
		"download_url":          ver + "terraform-provider-null_3.2.4_linux_amd64.zip",
		"shasums_url":           ver + "terraform-provider-null_3.2.4_SHA256SUMS",
		"shasums_signature_url": ver + "terraform-provider-null_3.2.4_SHA256SUMS.72D7468F.sig",
		"signing_keys":          map[string]any{"gpg_public_keys": []any{map[string]any{"key_id": "34365D9472D7468F"}}},
	})

	var doc map[string]any
	if err := json.Unmarshal(p.RewriteServed(body, remote, "https://aa.example:8443/"), &doc); err != nil {
		t.Fatal(err)
	}
	const prefix = "https://aa.example:8443/api/v1/remote/hashicorp-releases/"
	g := &generic.Provider{}
	for _, field := range []string{"download_url", "shasums_url", "shasums_signature_url"} {
		got, _ := doc[field].(string)
		path, ok := strings.CutPrefix(got, prefix)
		if !ok {
			t.Errorf("%s = %q, not routed through the releases remote", field, got)
			continue
		}
		if g.Classify(path) != provider.Immutable || !strings.HasPrefix(g.UpstreamURL(releases, path), ver) {
			t.Errorf("%s path %q must route back to %s as an immutable artifact", field, path, ver)
		}
	}
	if doc["os"] != "linux" || doc["signing_keys"] == nil {
		t.Errorf("other fields must survive the rewrite: %v", doc)
	}
	if _, ok := any(p).(provider.ResponseRewriter); ok {
		t.Error("terraform rewrites at serve time, not before caching")
	}
}
