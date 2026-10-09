package cargo

import (
	"encoding/json"
	"net/http/httptest"
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
		{"crates/serde/serde-1.0.200.crate", provider.Immutable},
		{"se/rd/serde", provider.Mutable},
		{"1/a", provider.Mutable},
		{"3/a/anyhow", provider.Mutable},
		{"config.json", provider.Mutable},
		{"cr/at/crates", provider.Mutable},
	}
	for _, tt := range tests {
		if got := p.Classify(tt.path); got != tt.want {
			t.Errorf("Classify(%q) = %v, want %v", tt.path, got, tt.want)
		}
	}
}

func TestUpstreamURL(t *testing.T) {
	p := &Provider{}
	cratesIO := models.Remote{BaseURL: "https://index.crates.io/"}
	mirror := models.Remote{BaseURL: "http://mirror.example"}
	tests := []struct {
		remote     models.Remote
		path, want string
	}{
		{cratesIO, "se/rd/serde", "https://index.crates.io/se/rd/serde"},
		{cratesIO, "crates/serde/serde-1.0.200.crate", "https://static.crates.io/crates/serde/serde-1.0.200.crate"},
		{mirror, "crates/serde/serde-1.0.200.crate", "http://mirror.example/crates/serde/serde-1.0.200.crate"},
		{mirror, "/2/ab", "http://mirror.example/2/ab"},
	}
	for _, tt := range tests {
		if got := p.UpstreamURL(tt.remote, tt.path); got != tt.want {
			t.Errorf("UpstreamURL(%q, %q) = %q, want %q", tt.remote.BaseURL, tt.path, got, tt.want)
		}
	}
}

func TestServeRemoteConfig(t *testing.T) {
	p := &Provider{}
	remote := models.Remote{Name: "crates-io", BaseURL: "https://index.crates.io"}

	w := httptest.NewRecorder()
	if !p.ServeRemote(w, httptest.NewRequest("GET", "/", nil), remote, "config.json", "https://aa.example/", nil) {
		t.Fatal("config.json not served")
	}
	var cfg map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &cfg); err != nil {
		t.Fatal(err)
	}
	want := "https://aa.example/api/v1/remote/crates-io/crates/{crate}/{crate}-{version}.crate"
	if cfg["dl"] != want {
		t.Errorf("dl = %q, want %q", cfg["dl"], want)
	}
	if _, ok := cfg["api"]; ok {
		t.Error("api must be omitted: publishing through the proxy is unsupported")
	}

	if p.ServeRemote(httptest.NewRecorder(), httptest.NewRequest("GET", "/", nil), remote, "se/rd/serde", "https://aa.example", nil) {
		t.Error("index files must fall through to the proxy engine")
	}
}
