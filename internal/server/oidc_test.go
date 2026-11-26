package server

import (
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/IUCLabs/artifactapi/internal/testsupport"
)

func TestServerOIDCWriteAuth(t *testing.T) {
	requireStack(t)
	iss := testsupport.NewOIDCIssuer()
	defer iss.Close()

	cfg := *testSrv.cfg
	cfg.OIDCIssuer = iss.URL
	cfg.OIDCAudiences = []string{"artifactapi"}
	s := *testSrv
	s.cfg = &cfg
	ts := httptest.NewServer(s.routes())
	defer ts.Close()

	good := iss.Token(nil, "artifactapi", "ci", time.Hour)
	do := func(method, path, body, token string) int {
		t.Helper()
		rq, _ := http.NewRequest(method, ts.URL+path, strings.NewReader(body))
		rq.Header.Set("Content-Type", "application/json")
		if token != "" {
			rq.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := http.DefaultClient.Do(rq)
		if err != nil {
			t.Fatalf("%s %s: %v", method, path, err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		return resp.StatusCode
	}
	expect := func(want int, method, path, body, token string) {
		t.Helper()
		if got := do(method, path, body, token); got != want {
			t.Errorf("%s %s: status %d, want %d", method, path, got, want)
		}
	}

	remote := `{"name":"srv-oidc","package_type":"generic","repo_type":"local"}`
	virtual := `{"name":"srv-oidc-v","package_type":"generic","members":["srv-oidc"]}`

	expect(401, "POST", "/api/v2/remotes", remote, "")
	expect(401, "POST", "/api/v2/remotes", remote, iss.Token(nil, "other", "ci", time.Hour))
	expect(201, "POST", "/api/v2/remotes", remote, good)
	expect(401, "PUT", "/api/v2/remotes/srv-oidc", `{"package_type":"generic"}`, "")
	expect(401, "DELETE", "/api/v2/remotes/srv-oidc", "", "")
	expect(401, "POST", "/api/v2/virtuals", virtual, "")
	expect(201, "POST", "/api/v2/virtuals", virtual, good)
	expect(401, "PUT", "/api/v2/virtuals/srv-oidc-v", virtual, "")
	expect(401, "DELETE", "/api/v2/virtuals/srv-oidc-v", "", "")
	expect(401, "PUT", "/api/v2/settings", `{"s3_health_down_threshold":4}`, "")
	expect(401, "PATCH", "/api/v2/settings", `{"s3_health_down_threshold":4}`, "")
	expect(401, "DELETE", "/api/v2/settings", "", "")
	expect(200, "GET", "/api/v2/settings", "", "")
	expect(200, "PUT", "/api/v2/settings", `{"s3_health_down_threshold":4}`, good)
	expect(200, "PATCH", "/api/v2/settings", `{"s3_health_up_threshold":2}`, good)
	expect(204, "DELETE", "/api/v2/settings", "", good)

	expect(200, "GET", "/api/v2/remotes", "", "")
	expect(200, "GET", "/api/v2/remotes/srv-oidc", "", "")
	expect(200, "GET", "/api/v2/virtuals/srv-oidc-v", "", "")
	expect(201, "PUT", "/api/v2/remotes/srv-oidc/files/a.bin", "payload", "")
	expect(200, "GET", "/api/v2/remotes/srv-oidc/files/a.bin", "", "")
	if got := do("DELETE", "/api/v2/remotes/srv-oidc/files/a.bin", "", ""); got >= 400 {
		t.Errorf("delete file: status %d", got)
	}
	if got := do("POST", "/api/v2/probe", `{}`, ""); got == 401 {
		t.Error("probe gated")
	}
	if got := do("GET", "/v2/", "", ""); got == 401 {
		t.Error("docker /v2 gated")
	}

	expect(204, "DELETE", "/api/v2/virtuals/srv-oidc-v", "", good)
	expect(204, "DELETE", "/api/v2/remotes/srv-oidc", "", good)
}

func TestServerAuthUploads(t *testing.T) {
	requireStack(t)
	iss := testsupport.NewOIDCIssuer()
	defer iss.Close()

	cfg := *testSrv.cfg
	cfg.OIDCIssuer = iss.URL
	cfg.OIDCAudiences = []string{"artifactapi"}
	cfg.AuthUploads = true
	s := *testSrv
	s.cfg = &cfg
	ts := httptest.NewServer(s.routes())
	defer ts.Close()

	good := iss.Token(nil, "artifactapi", "ci", time.Hour)
	do := func(method, path, body, authz string) int {
		t.Helper()
		rq, _ := http.NewRequest(method, ts.URL+path, strings.NewReader(body))
		rq.Header.Set("Content-Type", "application/json")
		if authz != "" {
			rq.Header.Set("Authorization", authz)
		}
		resp, err := http.DefaultClient.Do(rq)
		if err != nil {
			t.Fatalf("%s %s: %v", method, path, err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		return resp.StatusCode
	}
	bearer := "Bearer " + good
	for _, typ := range []string{"generic", "docker", "npm", "gems"} {
		body := `{"name":"srv-up-` + typ + `","package_type":"` + typ + `","repo_type":"local"}`
		if got := do("POST", "/api/v2/remotes", body, bearer); got != 201 {
			t.Fatalf("create %s: status %d", typ, got)
		}
	}

	writes := []struct{ method, path, body string }{
		{"PUT", "/api/v2/remotes/srv-up-generic/files/a.bin", "payload"},
		{"DELETE", "/api/v2/remotes/srv-up-generic/files/a.bin", ""},
		{"DELETE", "/api/v2/remotes/srv-up-generic/objects/a.bin", ""},
		{"DELETE", "/api/v2/locals/srv-up-generic/objects/a.bin", ""},
		{"PUT", "/api/v1/local/srv-up-npm/pkg", "{}"},
		{"POST", "/api/v1/local/srv-up-gems/api/v1/gems", "x"},
		{"DELETE", "/api/v1/local/srv-up-gems/api/v1/gems/yank", ""},
		{"POST", "/v2/srv-up-docker/img/blobs/uploads/", ""},
		{"PATCH", "/v2/srv-up-docker/img/blobs/uploads/x", ""},
		{"PUT", "/v2/srv-up-docker/img/manifests/t", "{}"},
		{"DELETE", "/v2/srv-up-docker/img/manifests/t", ""},
	}
	for _, w := range writes {
		if got := do(w.method, w.path, w.body, ""); got != 401 {
			t.Errorf("anonymous %s %s: status %d, want 401", w.method, w.path, got)
		}
	}

	if got := do("PUT", "/api/v2/remotes/srv-up-generic/files/a.bin", "payload", bearer); got != 201 {
		t.Errorf("bearer upload: status %d", got)
	}
	if got := do("PUT", "/api/v2/remotes/srv-up-generic/files/b.bin", "payload", basic(good)); got != 201 {
		t.Errorf("basic upload: status %d", got)
	}
	if got := do("POST", "/v2/srv-up-docker/img/blobs/uploads/", "", basic(good)); got != 202 {
		t.Errorf("basic docker upload: status %d", got)
	}
	expectOpen := func(method, path string) {
		t.Helper()
		if got := do(method, path, "", ""); got >= 400 {
			t.Errorf("anonymous %s %s: status %d", method, path, got)
		}
	}
	expectOpen("GET", "/api/v2/remotes/srv-up-generic/files/a.bin")
	expectOpen("GET", "/api/v2/remotes/srv-up-generic/objects")
	expectOpen("GET", "/api/v1/local/srv-up-generic/a.bin")
	expectOpen("GET", "/v2/token")
	if got := do("GET", "/v2/", "", ""); got != 401 {
		t.Errorf("anonymous ping: status %d, want 401 challenge", got)
	}
	if got := do("POST", "/api/v2/probe", `{}`, ""); got == 401 {
		t.Error("probe gated")
	}

	if got := do("DELETE", "/api/v2/remotes/srv-up-generic/files/a.bin", "", bearer); got >= 400 {
		t.Errorf("bearer delete: status %d", got)
	}
	for _, typ := range []string{"generic", "docker", "npm", "gems"} {
		do("DELETE", "/api/v2/remotes/srv-up-"+typ, "", bearer)
	}
}

func basic(tok string) string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte("ci:"+tok))
}
