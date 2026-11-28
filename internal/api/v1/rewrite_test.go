package v1

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/IUCLabs/artifactapi/internal/provider/nuget"
	"github.com/IUCLabs/artifactapi/pkg/models"
)

var nugetOrg = models.Remote{Name: "nuget-org", BaseURL: "https://api.nuget.org/v3/index.json"}

const serviceIndex = `{"resources":[{"@id":"https://api.nuget.org/v3-flatcontainer/"}]}`

func rewrite(t *testing.T, host, proto string) (*httptest.ResponseRecorder, string, bool) {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, "/api/v1/remote/nuget-org/index.json", nil)
	r.Host = host
	if proto != "" {
		r.Header.Set("X-Forwarded-Proto", proto)
	}
	w := httptest.NewRecorder()
	raw, ok := rewriteServed(w, r, &nuget.Provider{}, nugetOrg, strings.NewReader(serviceIndex))
	return w, string(raw), ok
}

func TestRewriteServedHeaders(t *testing.T) {
	w, body, ok := rewrite(t, "aa.example:8443", "https")
	if !ok {
		t.Fatal("valid request rejected")
	}
	if want := `{"resources":[{"@id":"https://aa.example:8443/api/v1/remote/nuget-org/v3-flatcontainer/"}]}`; body != want {
		t.Errorf("body = %s, want %s", body, want)
	}
	if got := w.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", got)
	}
	if got := w.Header().Get("Vary"); got != "Host, X-Forwarded-Proto" {
		t.Errorf("Vary = %q", got)
	}
	if got := w.Header().Get("Content-Length"); got != "91" || len(body) != 91 {
		t.Errorf("Content-Length = %q, body len %d", got, len(body))
	}
}

func TestRewriteServedIgnoresBogusProto(t *testing.T) {
	_, plain, _ := rewrite(t, "aa.example", "")
	_, bogus, _ := rewrite(t, "aa.example", "javascript")
	if bogus != plain || !strings.Contains(bogus, `"http://aa.example/`) {
		t.Errorf("bogus X-Forwarded-Proto changed the body: %s", bogus)
	}
}

func TestRewriteServedRejectsInvalidHost(t *testing.T) {
	w, _, ok := rewrite(t, `evil"},{"x":"`, "")
	if ok || w.Code != http.StatusBadRequest {
		t.Errorf("invalid host: ok=%v code=%d, want rejected with 400", ok, w.Code)
	}
}
