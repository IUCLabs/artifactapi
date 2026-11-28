package githubauth

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/IUCLabs/artifactapi/pkg/models"
)

type fakeAppStore struct {
	mu       sync.Mutex
	apps     map[string]models.GitHubApp
	fail     error
	keyLoads int
	checks   int
}

func (f *fakeAppStore) GetGitHubApp(_ context.Context, name string) (*models.GitHubApp, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.keyLoads++
	if f.fail != nil {
		return nil, f.fail
	}
	a, ok := f.apps[name]
	if !ok {
		return nil, models.ErrGitHubAppNotFound
	}
	return &a, nil
}

func (f *fakeAppStore) GitHubAppUpdatedAt(_ context.Context, name string) (time.Time, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.checks++
	if f.fail != nil {
		return time.Time{}, f.fail
	}
	a, ok := f.apps[name]
	if !ok {
		return time.Time{}, models.ErrGitHubAppNotFound
	}
	return a.UpdatedAt, nil
}

func (f *fakeAppStore) counts() (keyLoads, checks int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.keyLoads, f.checks
}

func (f *fakeAppStore) setFail(err error) {
	f.mu.Lock()
	f.fail = err
	f.mu.Unlock()
}

func (f *fakeAppStore) set(a models.GitHubApp) {
	f.mu.Lock()
	f.apps[a.Name] = a
	f.mu.Unlock()
}

func (f *fakeAppStore) del(name string) {
	f.mu.Lock()
	delete(f.apps, name)
	f.mu.Unlock()
}

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// newTestApps builds a registry that trusts and allowlists srv.
func newTestApps(store AppStore, srv *httptest.Server, clock *fakeClock) *Apps {
	reg := NewApps(store, []string{srv.Listener.Addr().String()})
	reg.SetHTTPClient(srv.Client())
	if clock != nil {
		reg.now = clock.now
	}
	return reg
}

func (a *Apps) cached(name string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	_, ok := a.cache[name]
	return ok
}

// installationServer mints "ghs_<installation>_<n>" for any installation id.
func installationServer(t *testing.T) (*httptest.Server, *int) {
	t.Helper()
	var mu sync.Mutex
	exchanges := 0
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		parts := strings.Split(r.URL.Path, "/")
		if len(parts) != 5 || parts[1] != "app" || parts[4] != "access_tokens" {
			http.NotFound(w, r)
			return
		}
		mu.Lock()
		exchanges++
		n := exchanges
		mu.Unlock()
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"token":      fmt.Sprintf("ghs_%s_%d", parts[3], n),
			"expires_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
		})
	}))
	t.Cleanup(srv.Close)
	return srv, &exchanges
}

func TestAppsCacheRevalidatesAfterTTL(t *testing.T) {
	srv, exchanges := installationServer(t)
	keyPEM := testRSAKeyPEM(t)
	t0 := time.Now()
	store := &fakeAppStore{apps: map[string]models.GitHubApp{}}
	store.set(models.GitHubApp{Name: "ci", AppID: 1, InstallationID: 100, PrivateKey: keyPEM, APIBaseURL: srv.URL, UpdatedAt: t0})
	clock := &fakeClock{t: t0}
	reg := newTestApps(store, srv, clock)
	ctx := context.Background()

	for range 3 {
		tok, err := AppToken(ctx, reg, "ci")
		if err != nil || tok != "ghs_100_1" {
			t.Fatalf("token = %q, %v; want ghs_100_1", tok, err)
		}
	}
	if loads, checks := store.counts(); loads != 1 || checks != 0 {
		t.Fatalf("within TTL: %d key loads, %d checks; want 1, 0", loads, checks)
	}

	clock.advance(AppRevalidateTTL)
	if tok, err := AppToken(ctx, reg, "ci"); err != nil || tok != "ghs_100_1" {
		t.Fatalf("unchanged after TTL token = %q, %v", tok, err)
	}
	if loads, checks := store.counts(); loads != 1 || checks != 1 {
		t.Fatalf("unchanged revalidation: %d key loads, %d checks; want 1, 1", loads, checks)
	}
	if *exchanges != 1 {
		t.Fatalf("unchanged app re-exchanged: %d exchanges", *exchanges)
	}

	store.set(models.GitHubApp{Name: "ci", AppID: 1, InstallationID: 200, PrivateKey: keyPEM, APIBaseURL: srv.URL, UpdatedAt: t0.Add(time.Second)})
	if tok, _ := AppToken(ctx, reg, "ci"); tok != "ghs_100_1" {
		t.Fatalf("update visible before TTL: %q", tok)
	}
	clock.advance(AppRevalidateTTL)
	if tok, err := AppToken(ctx, reg, "ci"); err != nil || tok != "ghs_200_2" {
		t.Fatalf("after update token = %q, %v; want ghs_200_2", tok, err)
	}
	if loads, _ := store.counts(); loads != 2 {
		t.Fatalf("changed updated_at: %d key loads; want 2", loads)
	}

	store.del("ci")
	clock.advance(AppRevalidateTTL)
	if _, err := AppToken(ctx, reg, "ci"); !errors.Is(err, models.ErrGitHubAppNotFound) {
		t.Fatalf("deleted app: %v; want not found", err)
	}
	if reg.cached("ci") {
		t.Fatal("deleted app left in cache")
	}
}

func TestAppsServeCachedOnTransientError(t *testing.T) {
	srv, _ := installationServer(t)
	store := &fakeAppStore{apps: map[string]models.GitHubApp{}}
	store.set(models.GitHubApp{Name: "ci", AppID: 1, InstallationID: 5, PrivateKey: testRSAKeyPEM(t), APIBaseURL: srv.URL, UpdatedAt: time.Now()})
	clock := &fakeClock{t: time.Now()}
	reg := newTestApps(store, srv, clock)
	ctx := context.Background()

	if _, err := AppToken(ctx, reg, "ci"); err != nil {
		t.Fatal(err)
	}
	store.setFail(errors.New("connection refused"))
	clock.advance(AppRevalidateTTL)
	if tok, err := AppToken(ctx, reg, "ci"); err != nil || tok != "ghs_5_1" {
		t.Fatalf("transient error: token = %q, %v; want cached ghs_5_1", tok, err)
	}
	if !reg.cached("ci") {
		t.Fatal("transient error dropped the cache entry")
	}
	_, checks := store.counts()
	var wg sync.WaitGroup
	for range 16 {
		wg.Go(func() {
			if tok, err := AppToken(ctx, reg, "ci"); err != nil || tok != "ghs_5_1" {
				t.Errorf("stale serve within TTL: %q, %v", tok, err)
			}
		})
	}
	wg.Wait()
	if _, c := store.counts(); c != checks {
		t.Fatalf("stale serve did not advance checkedAt: %d extra store checks", c-checks)
	}
	clock.advance(AppRevalidateTTL)
	for range 16 {
		wg.Go(func() {
			if _, err := AppToken(ctx, reg, "ci"); err != nil {
				t.Errorf("outage: %v", err)
			}
		})
	}
	wg.Wait()
	if _, c := store.counts(); c-checks != 1 {
		t.Fatalf("outage revalidations not collapsed: %d store checks for 16 callers", c-checks)
	}
	if _, err := AppToken(ctx, reg, "other"); err == nil {
		t.Fatal("uncached app with a failing store must error")
	}
}

func TestAppsConcurrentKeyRotation(t *testing.T) {
	srv, _ := installationServer(t)
	t0 := time.Now()
	store := &fakeAppStore{apps: map[string]models.GitHubApp{}}
	store.set(models.GitHubApp{Name: "ci", AppID: 1, InstallationID: 1, PrivateKey: testRSAKeyPEM(t), APIBaseURL: srv.URL, UpdatedAt: t0})
	newKey := testRSAKeyPEM(t)
	clock := &fakeClock{t: t0}
	reg := newTestApps(store, srv, clock)
	ctx := context.Background()

	const workers, calls = 32, 200
	var (
		mu       sync.Mutex
		seen     = map[string]map[*appCredential]bool{}
		progress atomic.Int64
		wg       sync.WaitGroup
	)
	record := func() string {
		c, err := reg.Credential(ctx, "ci")
		if err != nil {
			t.Errorf("credential: %v", err)
			return ""
		}
		ac := c.(*appCredential)
		mu.Lock()
		if seen[ac.installationID] == nil {
			seen[ac.installationID] = map[*appCredential]bool{}
		}
		seen[ac.installationID][ac] = true
		mu.Unlock()
		return ac.installationID
	}
	for range workers {
		wg.Go(func() {
			for range calls {
				record()
				progress.Add(1)
			}
		})
	}
	for progress.Load() < workers*calls/4 {
		runtime.Gosched()
	}
	store.set(models.GitHubApp{Name: "ci", AppID: 1, InstallationID: 2, PrivateKey: newKey, APIBaseURL: srv.URL, UpdatedAt: t0.Add(time.Second)})
	clock.advance(AppRevalidateTTL)
	wg.Wait()

	for range workers {
		wg.Go(func() {
			if id := record(); id != "2" {
				t.Errorf("old key served after TTL: installation %q", id)
			}
		})
	}
	wg.Wait()
	for id, creds := range seen {
		if len(creds) != 1 {
			t.Errorf("installation %s: %d credential instances; want 1 shared", id, len(creds))
		}
	}
	if len(seen) != 2 {
		t.Errorf("versions seen = %d; want 2", len(seen))
	}
	if loads, _ := store.counts(); loads != 2 {
		t.Errorf("key loads = %d; want one per version", loads)
	}
}

func TestAppsDeleteAcrossTTL(t *testing.T) {
	srv, _ := installationServer(t)
	t0 := time.Now()
	store := &fakeAppStore{apps: map[string]models.GitHubApp{}}
	app := models.GitHubApp{Name: "ci", AppID: 1, InstallationID: 1, PrivateKey: testRSAKeyPEM(t), APIBaseURL: srv.URL, UpdatedAt: t0}
	store.set(app)
	clock := &fakeClock{t: t0}
	reg := newTestApps(store, srv, clock)
	ctx := context.Background()

	first, err := reg.Credential(ctx, "ci")
	if err != nil {
		t.Fatal(err)
	}
	store.del("ci")
	clock.advance(AppRevalidateTTL - time.Millisecond)
	if c, err := reg.Credential(ctx, "ci"); err != nil || c != first {
		t.Fatalf("within TTL after delete: %v, %v; want cached credential", c, err)
	}
	clock.advance(time.Millisecond)
	if _, err := reg.Credential(ctx, "ci"); !errors.Is(err, models.ErrGitHubAppNotFound) {
		t.Fatalf("after TTL: %v; want not found", err)
	}
	if reg.cached("ci") {
		t.Fatal("deleted app left in cache")
	}
	if _, err := reg.Credential(ctx, "ci"); !errors.Is(err, models.ErrGitHubAppNotFound) {
		t.Fatalf("repeat after delete: %v; want not found", err)
	}

	app.UpdatedAt = t0.Add(time.Minute)
	store.set(app)
	again, err := reg.Credential(ctx, "ci")
	if err != nil || again == first {
		t.Fatalf("recreated app: %v, same instance %v; want a fresh credential", err, again == first)
	}
}

func TestAppsRefuseUnlistedStoredHost(t *testing.T) {
	srv, exchanges := installationServer(t)
	store := &fakeAppStore{apps: map[string]models.GitHubApp{}}
	store.set(models.GitHubApp{Name: "ci", AppID: 1, InstallationID: 1, PrivateKey: testRSAKeyPEM(t), APIBaseURL: srv.URL, UpdatedAt: time.Now()})
	reg := NewApps(store, []string{"ghe.example.com"})
	reg.SetHTTPClient(srv.Client())
	_, err := AppToken(context.Background(), reg, "ci")
	if err == nil || !strings.Contains(err.Error(), "GITHUB_APP_API_HOSTS") {
		t.Fatalf("unlisted stored host: %v; want a host policy error", err)
	}
	if reg.cached("ci") || *exchanges != 0 {
		t.Fatalf("unlisted host cached=%v exchanges=%d; want neither", reg.cached("ci"), *exchanges)
	}
}

func TestAppsRememberKeepsNewerEntry(t *testing.T) {
	reg := NewApps(&fakeAppStore{apps: map[string]models.GitHubApp{}}, nil)
	t0 := time.Now()
	newer, older, same := &appCredential{}, &appCredential{}, &appCredential{}
	reg.remember("ci", cachedApp{updatedAt: t0.Add(time.Second), checkedAt: t0, cred: newer})
	if got := reg.remember("ci", cachedApp{updatedAt: t0, checkedAt: t0, cred: older}); got != newer {
		t.Fatal("older read replaced a newer entry")
	}
	if got := reg.remember("ci", cachedApp{updatedAt: t0.Add(time.Second), checkedAt: t0.Add(time.Second), cred: same}); got != newer {
		t.Fatal("equal version replaced the cached credential")
	}
	if c := reg.cache["ci"]; c.cred != newer || !c.checkedAt.Equal(t0.Add(time.Second)) {
		t.Fatalf("equal version did not refresh checkedAt: %+v", c)
	}
}

func TestAppsRejectsBadStoredKey(t *testing.T) {
	store := &fakeAppStore{apps: map[string]models.GitHubApp{"bad": {Name: "bad", AppID: 1, InstallationID: 1, PrivateKey: "junk"}}}
	_, err := NewApps(store, nil).Credential(context.Background(), "bad")
	if err == nil || strings.Contains(err.Error(), "junk") {
		t.Fatalf("err = %v; want a key error without key material", err)
	}
}

func TestRemoteTokenPrecedence(t *testing.T) {
	srv, _ := installationServer(t)
	store := &fakeAppStore{apps: map[string]models.GitHubApp{}}
	store.set(models.GitHubApp{Name: "ci", AppID: 1, InstallationID: 7, PrivateKey: testRSAKeyPEM(t), APIBaseURL: srv.URL, UpdatedAt: time.Now()})
	reg := newTestApps(store, srv, nil)
	server := staticToken{token: "server"}
	ctx := context.Background()

	cases := []struct {
		name   string
		remote models.Remote
		server Credential
		want   string
	}{
		{"app beats everything", models.Remote{GitHubApp: "ci", Username: "u", Password: "p"}, server, "ghs_7_1"},
		{"password beats username and server", models.Remote{Username: "u", Password: "p"}, server, "p"},
		{"username beats server", models.Remote{Username: "u"}, server, "u"},
		{"server", models.Remote{}, server, "server"},
		{"anonymous", models.Remote{}, nil, ""},
	}
	for _, c := range cases {
		got, err := RemoteToken(ctx, reg, c.remote, c.server)
		if err != nil || got != c.want {
			t.Errorf("%s: got %q, %v; want %q", c.name, got, err, c.want)
		}
	}

	if _, err := RemoteToken(ctx, nil, models.Remote{GitHubApp: "ci"}, server); err == nil {
		t.Error("github_app without a registry must fail, not fall back")
	}
	if _, err := RemoteToken(ctx, reg, models.Remote{GitHubApp: "missing"}, server); err == nil {
		t.Error("missing github_app must fail, not fall back")
	}
}

func TestFingerprintStableAcrossEncodings(t *testing.T) {
	pkcs1 := testRSAKeyPEM(t)
	key, err := ParsePrivateKey(pkcs1)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	key8, err := ParsePrivateKey(string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})))
	if err != nil {
		t.Fatal(err)
	}
	a, _ := Fingerprint(key)
	b, _ := Fingerprint(key8)
	if a != b || !strings.HasPrefix(a, "SHA256:") || strings.HasSuffix(a, "=") || len(a) != len("SHA256:")+43 {
		t.Fatalf("fingerprints %q / %q", a, b)
	}
	other, _ := ParsePrivateKey(testRSAKeyPEM(t))
	if c, _ := Fingerprint(other); c == a {
		t.Fatal("different keys share a fingerprint")
	}
}
