package v2

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/IUCLabs/artifactapi/internal/auth"
	"github.com/IUCLabs/artifactapi/internal/database"
	"github.com/IUCLabs/artifactapi/internal/testsupport"
	"github.com/IUCLabs/artifactapi/pkg/models"
)

type memStates struct {
	mu   sync.Mutex
	now  time.Time
	vals map[string]memState
}

type memState struct {
	val     []byte
	expires time.Time
}

func (m *memStates) PutState(_ context.Context, key string, val []byte, ttl time.Duration) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.vals[key] = memState{val: val, expires: m.now.Add(ttl)}
	return nil
}

func (m *memStates) TakeState(_ context.Context, key string) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.vals[key]
	delete(m.vals, key)
	if !ok || !m.now.Before(s.expires) {
		return nil, nil
	}
	return s.val, nil
}

func (m *memStates) advance(d time.Duration) {
	m.mu.Lock()
	m.now = m.now.Add(d)
	m.mu.Unlock()
}

type memApps struct {
	mu   sync.Mutex
	apps map[string]models.GitHubApp
}

func (m *memApps) GetGitHubApp(_ context.Context, name string) (*models.GitHubApp, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	a, ok := m.apps[name]
	if !ok {
		return nil, database.ErrGitHubAppNotFound
	}
	return &a, nil
}

func (m *memApps) CreateGitHubApp(_ context.Context, a *models.GitHubApp) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.apps[a.Name]; ok {
		return database.ErrGitHubAppExists
	}
	a.UpdatedAt = time.Now()
	m.apps[a.Name] = *a
	return nil
}

func (m *memApps) PutGitHubApp(_ context.Context, a *models.GitHubApp) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	a.UpdatedAt = time.Now()
	m.apps[a.Name] = *a
	return nil
}

const (
	fakeAppID      = 4242
	webhookSecret  = "whsec-do-not-leak"
	clientSecret   = "client-secret-do-not-leak"
	ownInstall     = 99
	foreignInstall = 98
)

// fakeGitHub serves manifest conversion (code "good"; anything else 404s) and
// installation lookups authenticated by the app JWT, under /api/v3 as GHES does.
func fakeGitHub(t *testing.T, keyPEM string) *httptest.Server {
	t.Helper()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/v3/app-manifests/good/conversions":
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": fakeAppID, "slug": "artifactapi-ci", "pem": keyPEM, "client_id": "Iv1.abc",
				"client_secret": clientSecret, "webhook_secret": webhookSecret,
			})
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/api/v3/app/installations/"):
			tok, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
			parts := strings.Split(tok, ".")
			if len(parts) != 3 {
				http.Error(w, "no jwt", http.StatusUnauthorized)
				return
			}
			payload, _ := base64.RawURLEncoding.DecodeString(parts[1])
			var claims struct {
				Iss string `json:"iss"`
			}
			_ = json.Unmarshal(payload, &claims)
			if claims.Iss != "4242" {
				http.Error(w, "wrong app", http.StatusUnauthorized)
				return
			}
			switch strings.TrimPrefix(r.URL.Path, "/api/v3/app/installations/") {
			case "99":
				_ = json.NewEncoder(w).Encode(map[string]any{"id": ownInstall, "app_id": fakeAppID})
			case "98":
				_ = json.NewEncoder(w).Encode(map[string]any{"id": foreignInstall, "app_id": 1})
			default:
				http.NotFound(w, r)
			}
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

type manifestFixture struct {
	h      http.Handler
	iss    *testsupport.OIDCIssuer
	apps   *memApps
	states *memStates
	ghHost string
	keyPEM string
	logs   *bytes.Buffer
}

func newManifestFixture(t *testing.T) *manifestFixture {
	t.Helper()
	keyPEM := testAppKeyPEM(t)
	gh := fakeGitHub(t, keyPEM)
	ghHost := gh.Listener.Addr().String()
	iss := testsupport.NewOIDCIssuer()
	t.Cleanup(iss.Close)
	f := &manifestFixture{
		iss: iss, apps: &memApps{apps: map[string]models.GitHubApp{}},
		states: &memStates{now: time.Now(), vals: map[string]memState{}},
		ghHost: ghHost, keyPEM: keyPEM, logs: &bytes.Buffer{},
	}
	hosts := []string{ghHost, "ghe.example.com"}
	handler := NewGitHubAppsHandler(nil, hosts).
		WithManifest(NewGitHubAppManifestHandler(f.apps, f.states, hosts, gh.Client()))
	f.h = auth.NewBearerOIDC(iss.URL, nil).RequireWrites(handler.Routes())
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(f.logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return f
}

func (f *manifestFixture) post(t *testing.T, sub, path string, body any) (int, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(jsonBody(t, body)))
	req.Host = "artifactapi.example.com"
	req.Header.Set("X-Forwarded-Proto", "https")
	if sub != "" {
		req.Header.Set("Authorization", "Bearer "+f.iss.Token(nil, "artifactapi", sub, time.Hour))
	}
	w := httptest.NewRecorder()
	f.h.ServeHTTP(w, req)
	return w.Code, w.Body.String()
}

func decodeAs[T any](t *testing.T, body string) T {
	t.Helper()
	var v T
	if err := json.Unmarshal([]byte(body), &v); err != nil {
		t.Fatalf("decode %q: %v", body, err)
	}
	return v
}

func (f *manifestFixture) start(t *testing.T, sub string) manifestStartResponse {
	t.Helper()
	code, body := f.post(t, sub, "/manifest", map[string]any{"name": "ci", "github_host": f.ghHost})
	if code != http.StatusOK {
		t.Fatalf("start = %d %s", code, body)
	}
	return decodeAs[manifestStartResponse](t, body)
}

func (f *manifestFixture) convert(t *testing.T, sub, state string) manifestConvertResponse {
	t.Helper()
	code, body := f.post(t, sub, "/manifest/convert", map[string]any{"code": "good", "state": state})
	if code != http.StatusOK {
		t.Fatalf("convert = %d %s", code, body)
	}
	return decodeAs[manifestConvertResponse](t, body)
}

func installStateOf(t *testing.T, installURL string) string {
	t.Helper()
	u, err := url.Parse(installURL)
	if err != nil {
		t.Fatal(err)
	}
	return u.Query().Get("state")
}

func TestManifestStartContents(t *testing.T) {
	f := newManifestFixture(t)
	code, body := f.post(t, "alice", "/manifest", map[string]any{"name": "ci"})
	if code != http.StatusOK {
		t.Fatalf("start = %d %s", code, body)
	}
	resp := decodeAs[manifestStartResponse](t, body)
	if !stateRe.MatchString(resp.State) || resp.PostURL != "https://github.com/settings/apps/new?state="+resp.State {
		t.Fatalf("post_url = %q state = %q", resp.PostURL, resp.State)
	}
	m := resp.Manifest
	ui := "https://artifactapi.example.com/ui"
	if m.Name != "artifactapi-ci" || m.URL != ui+"/github-apps" || m.RedirectURL != ui+"/github-apps/manifest/callback" ||
		m.SetupURL != ui+"/github-apps/manifest/installed" || m.Public || m.RequestOAuthOnInstall ||
		m.HookAttributes["active"] || len(m.DefaultEvents) != 0 ||
		len(m.DefaultPermissions) != 2 || m.DefaultPermissions["contents"] != "read" || m.DefaultPermissions["metadata"] != "read" {
		t.Fatalf("manifest = %+v", m)
	}
	var raw map[string]map[string]any
	_ = json.Unmarshal([]byte(body), &raw)
	if ev, ok := raw["manifest"]["default_events"].([]any); !ok || len(ev) != 0 {
		t.Fatalf("default_events must serialise as [], got %v", raw["manifest"]["default_events"])
	}

	code, body = f.post(t, "alice", "/manifest", map[string]any{"name": "ci2", "owner": "IUCLabs", "github_app_name": "custom"})
	resp = decodeAs[manifestStartResponse](t, body)
	if code != http.StatusOK || resp.Manifest.Name != "custom" ||
		resp.PostURL != "https://github.com/organizations/IUCLabs/settings/apps/new?state="+resp.State {
		t.Fatalf("org start = %d %+v", code, resp)
	}
}

func TestManifestStartValidation(t *testing.T) {
	f := newManifestFixture(t)
	f.apps.apps["taken"] = models.GitHubApp{Name: "taken"}
	cases := []struct {
		name string
		body map[string]any
		want int
	}{
		{"bad name", map[string]any{"name": "-x"}, http.StatusBadRequest},
		{"reserved name", map[string]any{"name": "manifest"}, http.StatusBadRequest},
		{"name taken", map[string]any{"name": "taken"}, http.StatusConflict},
		{"bad owner", map[string]any{"name": "ok", "owner": "a/b"}, http.StatusBadRequest},
		{"github app name too long", map[string]any{"name": "ok", "github_app_name": strings.Repeat("x", 35)}, http.StatusBadRequest},
		{"unlisted ghes host", map[string]any{"name": "ok", "github_host": "evil.example.com"}, http.StatusBadRequest},
		{"api host is not a web host", map[string]any{"name": "ok", "github_host": "api.github.com"}, http.StatusBadRequest},
		{"host with path", map[string]any{"name": "ok", "github_host": "ghe.example.com/x"}, http.StatusBadRequest},
		{"unknown field", map[string]any{"name": "ok", "private_key": "x"}, http.StatusBadRequest},
	}
	for _, c := range cases {
		if code, body := f.post(t, "alice", "/manifest", c.body); code != c.want {
			t.Errorf("%s: %d %s, want %d", c.name, code, body, c.want)
		}
	}
	if code, _ := f.post(t, "", "/manifest", map[string]any{"name": "ok"}); code != http.StatusUnauthorized {
		t.Errorf("unauthenticated start = %d, want 401", code)
	}
	if len(f.states.vals) != 0 {
		t.Errorf("rejected starts stored %d states", len(f.states.vals))
	}
}

func TestManifestGHESEndpoints(t *testing.T) {
	f := newManifestFixture(t)
	code, body := f.post(t, "alice", "/manifest", map[string]any{"name": "ci", "github_host": "GHE.example.com", "owner": "eng"})
	resp := decodeAs[manifestStartResponse](t, body)
	if code != http.StatusOK || resp.PostURL != "https://ghe.example.com/organizations/eng/settings/apps/new?state="+resp.State {
		t.Fatalf("ghes start = %d %s", code, body)
	}
	if got := installURL("https://ghe.example.com", "a-b", "s"); got != "https://ghe.example.com/github-apps/a-b/installations/new?state=s" {
		t.Errorf("ghes install url = %q", got)
	}
	if got := installURL("https://github.com", "a-b", "s"); got != "https://github.com/apps/a-b/installations/new?state=s" {
		t.Errorf("github install url = %q", got)
	}
}

func TestManifestFullFlow(t *testing.T) {
	f := newManifestFixture(t)
	start := f.start(t, "alice")
	code, body := f.post(t, "alice", "/manifest/convert", map[string]any{"code": "good", "state": start.State})
	if code != http.StatusOK {
		t.Fatalf("convert = %d %s", code, body)
	}
	for _, secret := range []string{"PRIVATE KEY", webhookSecret, clientSecret} {
		if strings.Contains(body, secret) || strings.Contains(f.logs.String(), secret) {
			t.Fatalf("secret %q leaked into response or logs:\n%s\n%s", secret, body, f.logs.String())
		}
	}
	conv := decodeAs[manifestConvertResponse](t, body)
	wantPrefix := "https://" + f.ghHost + "/github-apps/artifactapi-ci/installations/new?state="
	if conv.Name != "ci" || conv.AppID != fakeAppID || conv.Slug != "artifactapi-ci" || !strings.HasPrefix(conv.InstallURL, wantPrefix) {
		t.Fatalf("convert = %+v", conv)
	}
	stored := f.apps.apps["ci"]
	if stored.AppID != fakeAppID || stored.InstallationID != 0 || stored.PrivateKey != f.keyPEM ||
		stored.APIBaseURL != "https://"+f.ghHost+"/api/v3" {
		t.Fatalf("stored = %+v", stored)
	}

	before := stored.UpdatedAt
	code, body = f.post(t, "alice", "/manifest/installed", map[string]any{"installation_id": ownInstall, "state": installStateOf(t, conv.InstallURL)})
	if code != http.StatusOK || strings.Contains(body, "PRIVATE KEY") {
		t.Fatalf("installed = %d %s", code, body)
	}
	if app := decodeAs[models.GitHubApp](t, body); app.Name != "ci" || app.InstallationID != ownInstall {
		t.Fatalf("installed app = %+v", app)
	}
	if after := f.apps.apps["ci"]; after.InstallationID != ownInstall || !after.UpdatedAt.After(before) {
		t.Fatalf("installation not saved: %+v", after)
	}
}

func TestManifestStateSingleUse(t *testing.T) {
	f := newManifestFixture(t)
	start := f.start(t, "alice")
	conv := f.convert(t, "alice", start.State)
	code, body := f.post(t, "alice", "/manifest/convert", map[string]any{"code": "good", "state": start.State})
	if code != http.StatusBadRequest || !strings.Contains(body, "expired or already used") {
		t.Fatalf("replayed convert = %d %s", code, body)
	}
	install := installStateOf(t, conv.InstallURL)
	if code, body := f.post(t, "alice", "/manifest/installed", map[string]any{"installation_id": ownInstall, "state": install}); code != http.StatusOK {
		t.Fatalf("installed = %d %s", code, body)
	}
	if code, _ := f.post(t, "alice", "/manifest/installed", map[string]any{"installation_id": ownInstall, "state": install}); code != http.StatusBadRequest {
		t.Fatalf("replayed installed = %d, want 400", code)
	}
	if code, _ := f.post(t, "alice", "/manifest/installed", map[string]any{"installation_id": ownInstall, "state": start.State}); code != http.StatusBadRequest {
		t.Fatalf("create state accepted for install step: %d", code)
	}
}

func TestManifestSubjectMismatch(t *testing.T) {
	f := newManifestFixture(t)
	start := f.start(t, "alice")
	if code, _ := f.post(t, "mallory", "/manifest/convert", map[string]any{"code": "good", "state": start.State}); code != http.StatusForbidden {
		t.Fatalf("other subject convert = %d, want 403", code)
	}
	if _, ok := f.apps.apps["ci"]; ok {
		t.Fatal("app stored for a mismatched subject")
	}
	start = f.start(t, "alice")
	conv := f.convert(t, "alice", start.State)
	code, _ := f.post(t, "mallory", "/manifest/installed", map[string]any{"installation_id": ownInstall, "state": installStateOf(t, conv.InstallURL)})
	if code != http.StatusForbidden || f.apps.apps["ci"].InstallationID != 0 {
		t.Fatalf("other subject installed = %d, installation %d", code, f.apps.apps["ci"].InstallationID)
	}
}

func TestManifestStateExpiry(t *testing.T) {
	f := newManifestFixture(t)
	start := f.start(t, "alice")
	f.states.advance(ManifestStateTTL + time.Second)
	code, body := f.post(t, "alice", "/manifest/convert", map[string]any{"code": "good", "state": start.State})
	if code != http.StatusBadRequest || !strings.Contains(body, "expired") {
		t.Fatalf("expired convert = %d %s", code, body)
	}

	start = f.start(t, "alice")
	conv := f.convert(t, "alice", start.State)
	f.states.advance(ManifestStateTTL + time.Second)
	if code, _ := f.post(t, "alice", "/manifest/installed", map[string]any{"installation_id": ownInstall, "state": installStateOf(t, conv.InstallURL)}); code != http.StatusBadRequest {
		t.Fatalf("expired installed = %d, want 400", code)
	}
}

func TestManifestConvertErrors(t *testing.T) {
	f := newManifestFixture(t)
	start := f.start(t, "alice")
	if code, _ := f.post(t, "alice", "/manifest/convert", map[string]any{"code": "../x", "state": start.State}); code != http.StatusBadRequest {
		t.Fatalf("bad code = %d", code)
	}
	code, body := f.post(t, "alice", "/manifest/convert", map[string]any{"code": "stale", "state": start.State})
	if code != http.StatusBadRequest || !strings.Contains(body, "GitHub rejected") {
		t.Fatalf("stale code = %d %s", code, body)
	}

	start = f.start(t, "alice")
	f.apps.apps["ci"] = models.GitHubApp{Name: "ci", AppID: 1}
	if code, _ := f.post(t, "alice", "/manifest/convert", map[string]any{"code": "good", "state": start.State}); code != http.StatusConflict {
		t.Fatalf("convert onto taken name = %d, want 409", code)
	}
	if f.apps.apps["ci"].AppID != 1 {
		t.Fatal("existing app overwritten")
	}
}

func TestManifestInstalledRejectsForeignInstallation(t *testing.T) {
	f := newManifestFixture(t)
	for _, id := range []int64{foreignInstall, 12345} {
		conv := f.convert(t, "alice", f.start(t, "alice").State)
		code, body := f.post(t, "alice", "/manifest/installed", map[string]any{"installation_id": id, "state": installStateOf(t, conv.InstallURL)})
		if code != http.StatusBadRequest || f.apps.apps["ci"].InstallationID != 0 {
			t.Fatalf("installation %d = %d %s; installation_id %d", id, code, body, f.apps.apps["ci"].InstallationID)
		}
		delete(f.apps.apps, "ci")
	}

	conv := f.convert(t, "alice", f.start(t, "alice").State)
	f.apps.apps["ci"] = models.GitHubApp{Name: "ci", AppID: 7, PrivateKey: f.keyPEM}
	if code, _ := f.post(t, "alice", "/manifest/installed", map[string]any{"installation_id": ownInstall, "state": installStateOf(t, conv.InstallURL)}); code != http.StatusConflict {
		t.Fatalf("replaced app installed = %d, want 409", code)
	}
}

func TestGitHubAppNameManifestReserved(t *testing.T) {
	if err := validGitHubAppName("manifest"); err == nil {
		t.Fatal(`"manifest" must be reserved: it collides with the manifest routes`)
	}
}
