package puppet

import (
	"context"
	"strings"
	"testing"

	"github.com/IUCLabs/artifactapi/internal/provider"
	"github.com/IUCLabs/artifactapi/pkg/models"
)

func TestType(t *testing.T) {
	if (&Provider{}).Type() != models.PackagePuppet {
		t.Fatal("wrong type")
	}
}

func TestClassify(t *testing.T) {
	p := &Provider{}
	if p.Classify("v3/modules/puppetlabs-stdlib") != provider.Mutable {
		t.Error("modules should be mutable")
	}
	if p.Classify("v3/releases?module=x") != provider.Mutable {
		t.Error("releases should be mutable")
	}
	if p.Classify("v3/files/puppetlabs-stdlib-1.0.0.tar.gz") != provider.Immutable {
		t.Error("files should be immutable")
	}
}

func TestContentType(t *testing.T) {
	p := &Provider{}
	if p.ContentType("x/mod-1.0.0.tar.gz") != "application/gzip" {
		t.Error("tar.gz")
	}
	if p.ContentType("v3/modules/x") != "application/json" {
		t.Error("v3 json")
	}
	if p.ContentType("other") != "application/octet-stream" {
		t.Error("default")
	}
}

func TestUpstreamURL(t *testing.T) {
	got := (&Provider{}).UpstreamURL(models.Remote{BaseURL: "https://forgeapi.puppet.com/"}, "/v3/modules/x")
	if got != "https://forgeapi.puppet.com/v3/modules/x" {
		t.Errorf("got %q", got)
	}
}

func TestRewriteResponse(t *testing.T) {
	p := &Provider{}
	remote := models.Remote{Name: "forge", BaseURL: "https://forgeapi.puppet.com"}

	if out, _ := p.RewriteResponse([]byte("x"), remote, ""); out != nil {
		t.Error("empty proxyBaseURL is a no-op")
	}

	body := []byte(`{"file_uri":"/v3/files/mod.tar.gz","home":"https://forgeapi.puppet.com/x"}`)
	out, err := p.RewriteResponse(body, remote, "http://proxy")
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	if !strings.Contains(s, "http://proxy/api/v1/remote/forge/v3/files/mod.tar.gz") {
		t.Errorf("v3/files not rewritten: %s", s)
	}
	if !strings.Contains(s, "http://proxy/api/v1/remote/forge/x") {
		t.Errorf("base URL not rewritten: %s", s)
	}
}

func TestAuthHeaders(t *testing.T) {
	h, _ := (&Provider{}).AuthHeaders(context.Background(), models.Remote{})
	if h.Get("Authorization") != "" {
		t.Error("no credentials, no header")
	}
}

func TestForwardedQuery(t *testing.T) {
	p := &Provider{}
	if p.ForwardedQuery("v3/files/a-b-1.0.0.tar.gz") != nil || p.ForwardedQuery("v3/modules/a-b") != nil {
		t.Fatal("only release listings forward a query")
	}
	f := p.ForwardedQuery("v3/releases")
	for name, c := range map[string]struct {
		in, want string
		ok       bool
	}{
		"module": {"Puppetlabs/stdlib", "puppetlabs-stdlib", true},
		"limit":  {"500", "100", true},
		"offset": {"40", "40", true},
	} {
		if got, ok := f[name](c.in); got != c.want || ok != c.ok {
			t.Errorf("%s(%q) = %q %v, want %q %v", name, c.in, got, ok, c.want, c.ok)
		}
	}
	for name, bad := range map[string]string{"module": "../x", "limit": "0", "offset": "-1"} {
		if _, ok := f[name](bad); ok {
			t.Errorf("%s(%q) accepted", name, bad)
		}
	}
}
