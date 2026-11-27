package alpine

import (
	"context"
	"testing"

	"github.com/IUCLabs/artifactapi/internal/provider"
	"github.com/IUCLabs/artifactapi/pkg/models"
)

func TestType(t *testing.T) {
	if (&Provider{}).Type() != models.PackageAlpine {
		t.Fatal("wrong type")
	}
}

func TestClassify(t *testing.T) {
	p := &Provider{}
	if p.Classify("v3.19/main/x86_64/APKINDEX.tar.gz") != provider.Mutable {
		t.Error("APKINDEX should be mutable")
	}
	if p.Classify("v3.19/main/x86_64/curl-8.0-r0.apk") != provider.Immutable {
		t.Error("apk should be immutable")
	}
}

func TestContentType(t *testing.T) {
	p := &Provider{}
	cases := map[string]string{
		"pkg.apk":          "application/vnd.android.package-archive",
		"APKINDEX.tar.gz":  "application/gzip",
		"something.random": "application/octet-stream",
	}
	for path, want := range cases {
		if got := p.ContentType(path); got != want {
			t.Errorf("ContentType(%q) = %q, want %q", path, got, want)
		}
	}
}

func TestUpstreamURL(t *testing.T) {
	p := &Provider{}
	got := p.UpstreamURL(models.Remote{BaseURL: "https://dl-cdn.alpinelinux.org/alpine/"}, "/v3.19/main/x86_64/curl.apk")
	if got != "https://dl-cdn.alpinelinux.org/alpine/v3.19/main/x86_64/curl.apk" {
		t.Errorf("got %q", got)
	}
}

func TestAuthHeaders(t *testing.T) {
	h, _ := (&Provider{}).AuthHeaders(context.Background(), models.Remote{Username: "u", Password: "p"})
	if h.Get("Authorization") == "" {
		t.Error("expected auth header")
	}
}
