package v2

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/IUCLabs/artifactapi/internal/database"
	"github.com/IUCLabs/artifactapi/pkg/models"
)

func testAppKeyPEM(t *testing.T) string {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))
}

func jsonBody(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func call(t *testing.T, h http.Handler, method, path, body string) (int, string) {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w.Code, w.Body.String()
}

func testDBOrSkip(t *testing.T) *database.DB {
	t.Helper()
	if testDSN == "" {
		t.Skip("Docker unavailable")
	}
	db, err := database.New(testDSN)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(db.Close)
	return db
}

func TestGitHubAppsAPI(t *testing.T) {
	db := testDBOrSkip(t)
	h := NewGitHubAppsHandler(db, []string{"ghe.example.com"}).Routes()
	keyPEM := testAppKeyPEM(t)
	keyLine := strings.Split(keyPEM, "\n")[1]
	valid := map[string]any{"app_id": 11, "installation_id": 22, "private_key": keyPEM, "description": "ci"}

	code, body := call(t, h, "PUT", "/api-app", jsonBody(t, valid))
	if code != 200 {
		t.Fatalf("put = %d %s", code, body)
	}
	t.Cleanup(func() { _ = db.DeleteGitHubApp(t.Context(), "api-app") })
	var created models.GitHubApp
	if err := json.Unmarshal([]byte(body), &created); err != nil {
		t.Fatal(err)
	}
	if created.APIBaseURL != models.DefaultGitHubAPIBaseURL || created.AppID != 11 || created.InstallationID != 22 ||
		!strings.HasPrefix(created.PrivateKeyFingerprint, "SHA256:") || created.CreatedAt.IsZero() {
		t.Fatalf("created = %+v", created)
	}

	code, body = call(t, h, "PUT", "/api-app", jsonBody(t, valid))
	var again models.GitHubApp
	_ = json.Unmarshal([]byte(body), &again)
	if code != 200 || again.PrivateKeyFingerprint != created.PrivateKeyFingerprint || !again.UpdatedAt.Equal(created.UpdatedAt) {
		t.Fatalf("idempotent put = %d %+v", code, again)
	}

	for _, path := range []string{"/", "/api-app"} {
		code, body = call(t, h, "GET", path, "")
		if code != 200 || strings.Contains(body, keyLine) || strings.Contains(body, "private_key\"") ||
			!strings.Contains(body, created.PrivateKeyFingerprint) {
			t.Fatalf("GET %s = %d %s", path, code, body)
		}
	}

	with := func(k string, v any) map[string]any {
		m := map[string]any{}
		for kk, vv := range valid {
			m[kk] = vv
		}
		if v == nil {
			delete(m, k)
		} else {
			m[k] = v
		}
		return m
	}
	for name, b := range map[string]string{
		"missing app_id":      jsonBody(t, with("app_id", nil)),
		"zero installation":   jsonBody(t, with("installation_id", 0)),
		"missing private_key": jsonBody(t, with("private_key", nil)),
		"garbage key":         jsonBody(t, with("private_key", "-----BEGIN RSA PRIVATE KEY-----\nAAAA\n-----END RSA PRIVATE KEY-----\n")),
		"http api base":       jsonBody(t, with("api_base_url", "http://api.github.com")),
		"unlisted api host":   jsonBody(t, with("api_base_url", "https://evil.example.com")),
		"unknown field":       jsonBody(t, with("owner", "x")),
		"not json":            "{",
	} {
		if code, _ := call(t, h, "PUT", "/api-app", b); code != 400 {
			t.Errorf("%s: PUT = %d, want 400", name, code)
		}
	}
	if code, body := call(t, h, "PUT", "/ghes-app", jsonBody(t, with("api_base_url", "https://ghe.example.com/api/v3"))); code != 200 {
		t.Errorf("allowlisted GHES host PUT = %d %s, want 200", code, body)
	}
	t.Cleanup(func() { _ = db.DeleteGitHubApp(t.Context(), "ghes-app") })
	if code, _ := call(t, h, "PUT", "/bad%20name", jsonBody(t, valid)); code != 400 {
		t.Errorf("invalid name PUT = %d, want 400", code)
	}
	if code, _ := call(t, h, "GET", "/nope", ""); code != 404 {
		t.Errorf("GET missing = %d, want 404", code)
	}
	if code, _ := call(t, h, "DELETE", "/nope", ""); code != 404 {
		t.Errorf("DELETE missing = %d, want 404", code)
	}
}

func TestRemoteGitHubAppReference(t *testing.T) {
	db := testDBOrSkip(t)
	apps := NewGitHubAppsHandler(db, []string{"ghe.example.com"}).Routes()
	remotes := NewRemotesHandler(db, nil, nil).Routes()
	if code, body := call(t, apps, "PUT", "/ref-api-app", jsonBody(t, map[string]any{
		"app_id": 1, "installation_id": 2, "private_key": testAppKeyPEM(t),
	})); code != 200 {
		t.Fatalf("put app = %d %s", code, body)
	}
	t.Cleanup(func() {
		_ = db.DeleteRemote(t.Context(), "ref-api-remote")
		_ = db.DeleteGitHubApp(t.Context(), "ref-api-app")
	})

	const gh = `"package_type":"generic","base_url":"https://github.com"`
	for name, b := range map[string]string{
		"missing app":        `{"name":"ref-api-bad",` + gh + `,"github_app":"no-such-app"}`,
		"non-github type":    `{"name":"ref-api-bad","package_type":"rpm","base_url":"https://mirror.example.com","github_app":"ref-api-app"}`,
		"non-github generic": `{"name":"ref-api-bad","package_type":"generic","base_url":"https://example.com","github_app":"ref-api-app"}`,
	} {
		if code, body := call(t, remotes, "POST", "/", b); code != 400 {
			t.Errorf("%s: create = %d %s, want 400", name, code, body)
		}
	}

	code, body := call(t, remotes, "POST", "/", `{"name":"ref-api-remote",`+gh+`,"github_app":"ref-api-app"}`)
	if code != 201 || !strings.Contains(body, `"github_app":"ref-api-app"`) {
		t.Fatalf("create = %d %s", code, body)
	}
	if code, body := call(t, remotes, "GET", "/ref-api-remote", ""); code != 200 || !strings.Contains(body, `"github_app":"ref-api-app"`) {
		t.Fatalf("get = %d %s", code, body)
	}

	code, body = call(t, apps, "DELETE", "/ref-api-app", "")
	if code != 409 || !strings.Contains(body, "ref-api-remote") {
		t.Fatalf("delete referenced = %d %s, want 409 naming the remote", code, body)
	}

	if code, body := call(t, remotes, "PUT", "/ref-api-remote", `{`+gh+`}`); code != 200 || strings.Contains(body, "github_app") {
		t.Fatalf("clear github_app = %d %s", code, body)
	}
	if code, _ := call(t, apps, "DELETE", "/ref-api-app", ""); code != 204 {
		t.Fatalf("delete unreferenced = %d, want 204", code)
	}
}

type failingAppStore struct{ GitHubAppStore }

func (failingAppStore) PutGitHubApp(context.Context, *models.GitHubApp) error {
	return errors.New("db exploded: secret detail")
}

func TestGitHubAppsPutHidesStoreError(t *testing.T) {
	h := NewGitHubAppsHandler(failingAppStore{}, nil).Routes()
	code, body := call(t, h, "PUT", "/ci", jsonBody(t, map[string]any{"app_id": 1, "installation_id": 2, "private_key": testAppKeyPEM(t)}))
	if code != 500 || strings.Contains(body, "secret detail") {
		t.Fatalf("PUT = %d %q; want 500 without the store error", code, body)
	}
}
