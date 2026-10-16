package server

import (
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
