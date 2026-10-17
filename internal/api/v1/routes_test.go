package v1

import (
	"net/http"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/IUCLabs/artifactapi/internal/provider"
	"github.com/IUCLabs/artifactapi/internal/provider/npm"
	"github.com/IUCLabs/artifactapi/internal/provider/nuget"
	"github.com/IUCLabs/artifactapi/internal/provider/rpm"
)

func TestRoutesRemoteHead(t *testing.T) {
	r := (&ProxyHandler{}).Routes()
	for _, m := range []string{http.MethodGet, http.MethodHead} {
		if !r.Match(chi.NewRouteContext(), m, "/remote/myremote/some/path") {
			t.Errorf("%s /remote/myremote/some/path not routed", m)
		}
	}
}

func TestRewritesMutable(t *testing.T) {
	tests := []struct {
		name string
		prov provider.Provider
		path string
		want bool
	}{
		{"npm packument", &npm.Provider{}, "left-pad", true},
		{"npm tarball", &npm.Provider{}, "left-pad/-/left-pad-1.3.0.tgz", false},
		{"nuget index", &nuget.Provider{}, "index.json", true},
		{"rpm repodata", &rpm.Provider{}, "repodata/repomd.xml", false},
	}
	for _, tc := range tests {
		if got := rewritesMutable(tc.prov, tc.path); got != tc.want {
			t.Errorf("%s: rewritesMutable = %v, want %v", tc.name, got, tc.want)
		}
	}
}
