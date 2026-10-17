package nuget

import (
	"testing"

	"github.com/IUCLabs/artifactapi/internal/provider"
	"github.com/IUCLabs/artifactapi/pkg/models"
)

var _ provider.ServeRewriter = (*Provider)(nil)

var nugetOrg = models.Remote{Name: "nuget-org", BaseURL: "https://api.nuget.org/v3/index.json"}

func TestClassify(t *testing.T) {
	p := &Provider{}
	tests := []struct {
		path string
		want provider.Mutability
	}{
		{"v3-flatcontainer/newtonsoft.json/13.0.3/newtonsoft.json.13.0.3.nupkg", provider.Immutable},
		{"v3-flatcontainer/newtonsoft.json/13.0.3/newtonsoft.json.nuspec", provider.Immutable},
		{"v3-flatcontainer/newtonsoft.json/index.json", provider.Mutable},
		{"v3/registration5-gz-semver2/newtonsoft.json/index.json", provider.Mutable},
		{"index.json", provider.Mutable},
	}
	for _, tt := range tests {
		if got := p.Classify(tt.path); got != tt.want {
			t.Errorf("Classify(%q) = %v, want %v", tt.path, got, tt.want)
		}
	}
}

func TestUpstreamURL(t *testing.T) {
	p := &Provider{}
	tests := []struct {
		path, want string
	}{
		{"index.json", "https://api.nuget.org/v3/index.json"},
		{"/index.json", "https://api.nuget.org/v3/index.json"},
		{"v3-flatcontainer/serilog/index.json", "https://api.nuget.org/v3-flatcontainer/serilog/index.json"},
		{"v3/registration5-gz-semver2/serilog/index.json", "https://api.nuget.org/v3/registration5-gz-semver2/serilog/index.json"},
	}
	for _, tt := range tests {
		if got := p.UpstreamURL(nugetOrg, tt.path); got != tt.want {
			t.Errorf("UpstreamURL(%q) = %q, want %q", tt.path, got, tt.want)
		}
	}
}

func TestRewriteServed(t *testing.T) {
	p := &Provider{}
	body := `{"resources":[` +
		`{"@id":"https://api.nuget.org/v3-flatcontainer/","@type":"PackageBaseAddress/3.0.0"},` +
		`{"@id":"https://azuresearch-usnc.nuget.org/query","@type":"SearchQueryService"}]}`
	want := `{"resources":[` +
		`{"@id":"http://aa.example:8000/api/v1/remote/nuget-org/v3-flatcontainer/","@type":"PackageBaseAddress/3.0.0"},` +
		`{"@id":"https://azuresearch-usnc.nuget.org/query","@type":"SearchQueryService"}]}`
	if got := string(p.RewriteServed([]byte(body), nugetOrg, "http://aa.example:8000/")); got != want {
		t.Errorf("RewriteServed =\n%s\nwant\n%s", got, want)
	}
	if got := string(p.RewriteServed([]byte(body), models.Remote{Name: "x"}, "http://aa.example")); got != body {
		t.Error("remote without a parseable base_url must pass the body through")
	}
	cert := "\x30\x82https://api.nuget.org"
	if got := string(p.RewriteServed([]byte(cert), nugetOrg, "http://aa.example")); got != cert {
		t.Error("non-JSON bodies must pass through untouched")
	}
}
