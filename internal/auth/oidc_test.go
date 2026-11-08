package auth

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/containerd/containerd/v2/core/remotes"
	"github.com/containerd/containerd/v2/core/remotes/docker"
	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
	"github.com/opencontainers/go-digest"
	specs "github.com/opencontainers/image-spec/specs-go"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/IUCLabs/artifactapi/internal/testsupport"
)

func serve(h http.Handler, method, token string) int {
	if token == "" {
		return serveAuth(h, method, "")
	}
	return serveAuth(h, method, "Bearer "+token)
}

func serveAuth(h http.Handler, method, authz string) int {
	r := httptest.NewRequest(method, "/x", nil)
	if authz != "" {
		r.Header.Set("Authorization", authz)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w.Code
}

func claims(iss string) jwt.Claims {
	now := time.Now()
	return jwt.Claims{
		Issuer:   iss,
		Subject:  "ci",
		Audience: jwt.Audience{"artifactapi"},
		IssuedAt: jwt.NewNumericDate(now.Add(-time.Minute)),
		Expiry:   jwt.NewNumericDate(now.Add(time.Hour)),
	}
}

func hs256Token(t *testing.T, c jwt.Claims) string {
	sig, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.HS256, Key: []byte("0123456789abcdef0123456789abcdef")},
		(&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", "k1"))
	if err != nil {
		t.Fatal(err)
	}
	tok, err := jwt.Signed(sig).Claims(c).Serialize()
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

func noneToken(iss string) string {
	enc := base64.RawURLEncoding.EncodeToString
	return enc([]byte(`{"alg":"none","typ":"JWT","kid":"k1"}`)) + "." +
		enc([]byte(`{"iss":"`+iss+`","sub":"ci","aud":"artifactapi","exp":`+strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10)+`}`)) + "."
}

var ok = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) })

func TestBearerOIDC(t *testing.T) {
	iss := testsupport.NewOIDCIssuer()
	defer iss.Close()
	h := NewBearerOIDC(iss.URL, []string{"other", "artifactapi"}).Require(ok)

	otherKey, _ := rsa.GenerateKey(rand.Reader, 2048)
	cases := []struct {
		name  string
		token string
		want  int
	}{
		{"valid", iss.Token(nil, "artifactapi", "ci", time.Hour), http.StatusNoContent},
		{"missing", "", http.StatusUnauthorized},
		{"garbage", "not.a.jwt", http.StatusUnauthorized},
		{"bad signature", iss.Token(otherKey, "artifactapi", "ci", time.Hour), http.StatusUnauthorized},
		{"wrong aud", iss.Token(nil, "nope", "ci", time.Hour), http.StatusUnauthorized},
		{"expired", iss.Token(nil, "artifactapi", "ci", -time.Hour), http.StatusUnauthorized},
		{"wrong iss", iss.Sign(nil, claims("https://evil.example")), http.StatusUnauthorized},
		{"hs256", hs256Token(t, claims(iss.URL)), http.StatusUnauthorized},
		{"alg none", noneToken(iss.URL), http.StatusUnauthorized},
	}
	for _, c := range cases {
		if got := serve(h, http.MethodPost, c.token); got != c.want {
			t.Errorf("%s: status %d, want %d", c.name, got, c.want)
		}
	}

	valid := iss.Token(nil, "artifactapi", "ci", time.Hour)
	headers := []struct {
		name  string
		authz string
		want  int
	}{
		{"lowercase scheme", "bearer " + valid, http.StatusNoContent},
		{"extra whitespace", "  Bearer   " + valid + " ", http.StatusNoContent},
		{"empty value", "Bearer ", http.StatusUnauthorized},
		{"scheme only", "Bearer", http.StatusUnauthorized},
		{"basic scheme", "Basic " + valid, http.StatusUnauthorized},
	}
	for _, c := range headers {
		if got := serveAuth(h, http.MethodPost, c.authz); got != c.want {
			t.Errorf("%s: status %d, want %d", c.name, got, c.want)
		}
	}
}

func TestBearerOIDCHangingIssuerBacksOff(t *testing.T) {
	var hits atomic.Int32
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer srv.Close()
	defer close(release)

	b := NewBearerOIDC(srv.URL, nil)
	b.timeout = 100 * time.Millisecond
	b.backoff = time.Hour
	h := b.Require(ok)

	start := time.Now()
	if got := serve(h, http.MethodPost, "x.y.z"); got != http.StatusServiceUnavailable {
		t.Fatalf("hanging issuer: status %d, want 503", got)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("hanging issuer: took %s, timeout not applied", d)
	}
	if got := serve(h, http.MethodPost, "x.y.z"); got != http.StatusServiceUnavailable {
		t.Fatalf("during backoff: status %d, want 503", got)
	}
	if n := hits.Load(); n != 1 {
		t.Fatalf("issuer hit %d times, want 1", n)
	}
}

func TestBearerOIDCPanickingDiscoveryBacksOff(t *testing.T) {
	b := NewBearerOIDC("http://issuer.invalid", nil)
	b.backoff = time.Hour
	b.discover = func(context.Context, string) (*oidc.Provider, error) { panic("boom") }
	h := b.Require(ok)

	func() {
		defer func() { _ = recover() }()
		serve(h, http.MethodPost, "x.y.z")
	}()

	got := make(chan int, 1)
	go func() { got <- serve(h, http.MethodPost, "x.y.z") }()
	select {
	case code := <-got:
		if code != http.StatusServiceUnavailable {
			t.Fatalf("after panic: status %d, want 503", code)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("after panic: request hung on stale inflight discovery")
	}
}

func TestBearerOIDCConcurrentFirstRequestsWaitForDiscovery(t *testing.T) {
	iss := testsupport.NewOIDCIssuer()
	defer iss.Close()
	var hits atomic.Int32
	inner := iss.Config.Handler
	iss.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/.well-known/openid-configuration" {
			hits.Add(1)
			time.Sleep(200 * time.Millisecond)
		}
		inner.ServeHTTP(w, r)
	})

	h := NewBearerOIDC(iss.URL, nil).Require(ok)
	tok := iss.Token(nil, "artifactapi", "ci", time.Hour)
	var wg sync.WaitGroup
	codes := make([]int, 20)
	for i := range codes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			codes[i] = serve(h, http.MethodPost, tok)
		}()
	}
	wg.Wait()
	for i, c := range codes {
		if c != http.StatusNoContent {
			t.Errorf("request %d: status %d, want 204", i, c)
		}
	}
	if n := hits.Load(); n != 1 {
		t.Fatalf("discovery hit %d times, want 1", n)
	}
}

func TestBearerOIDCRetriesAfterBackoff(t *testing.T) {
	iss := testsupport.NewOIDCIssuer()
	defer iss.Close()
	b := NewBearerOIDC(iss.URL, nil)
	b.failedAt = time.Now()
	b.backoff = 50 * time.Millisecond
	tok := iss.Token(nil, "artifactapi", "ci", time.Hour)
	if got := serve(b.Require(ok), http.MethodPost, tok); got != http.StatusServiceUnavailable {
		t.Fatalf("within backoff: status %d, want 503", got)
	}
	time.Sleep(60 * time.Millisecond)
	if got := serve(b.Require(ok), http.MethodPost, tok); got != http.StatusNoContent {
		t.Fatalf("after backoff: status %d, want 204", got)
	}
}

func TestBearerOIDCIssuerDown(t *testing.T) {
	iss := testsupport.NewOIDCIssuer()
	b := NewBearerOIDC(iss.URL, []string{"artifactapi"})
	tok := iss.Token(nil, "artifactapi", "ci", time.Hour)
	iss.Close()
	if got := serve(b.Require(ok), http.MethodPost, tok); got != http.StatusServiceUnavailable {
		t.Errorf("issuer down: status %d, want 503", got)
	}
}

func TestBearerOIDCDisabled(t *testing.T) {
	if got := serve(NewBearerOIDC("", nil).RequireWrites(ok), http.MethodDelete, ""); got != http.StatusNoContent {
		t.Errorf("disabled: status %d, want 204", got)
	}
}

func TestRequireWritesLeavesReadsOpen(t *testing.T) {
	iss := testsupport.NewOIDCIssuer()
	defer iss.Close()
	h := NewBearerOIDC(iss.URL, []string{"artifactapi"}).RequireWrites(ok)
	for _, m := range []string{http.MethodGet, http.MethodHead, http.MethodOptions} {
		if got := serve(h, m, ""); got != http.StatusNoContent {
			t.Errorf("%s: status %d, want 204", m, got)
		}
	}
	for _, m := range []string{http.MethodPost, http.MethodPut, http.MethodDelete} {
		if got := serve(h, m, ""); got != http.StatusUnauthorized {
			t.Errorf("%s: status %d, want 401", m, got)
		}
	}
}

func basic(user, pass string) string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(user+":"+pass))
}

func TestTokenSchemes(t *testing.T) {
	iss := testsupport.NewOIDCIssuer()
	defer iss.Close()
	h := NewBearerOIDC(iss.URL, []string{"artifactapi"}).RequireWrites(ok)
	valid := iss.Token(nil, "artifactapi", "ci", time.Hour)
	cases := []struct {
		name  string
		authz string
		want  int
	}{
		{"bearer", "Bearer " + valid, http.StatusNoContent},
		{"token scheme", "Token " + valid, http.StatusNoContent},
		{"basic password", basic("anyone", valid), http.StatusNoContent},
		{"basic empty user", basic("", valid), http.StatusNoContent},
		{"bare jwt", valid, http.StatusNoContent},
		{"basic wrong aud", basic("ci", iss.Token(nil, "nope", "ci", time.Hour)), http.StatusUnauthorized},
		{"basic token as user", basic(valid, "x"), http.StatusUnauthorized},
		{"bare garbage", "garbage", http.StatusUnauthorized},
		{"unknown scheme", "Digest " + valid, http.StatusUnauthorized},
	}
	for _, c := range cases {
		if got := serveAuth(h, http.MethodPut, c.authz); got != c.want {
			t.Errorf("%s: status %d, want %d", c.name, got, c.want)
		}
	}
}

func TestRequireRegistryWrites(t *testing.T) {
	iss := testsupport.NewOIDCIssuer()
	defer iss.Close()
	h := NewBearerOIDC(iss.URL, []string{"artifactapi"}).RequireRegistryWrites(ok, func(*http.Request) string { return "https://reg.example/v2/token" })
	valid := iss.Token(nil, "artifactapi", "ci", time.Hour)
	do := func(method, path, authz string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, nil)
		if authz != "" {
			r.Header.Set("Authorization", authz)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	challenge := `Bearer realm="https://reg.example/v2/token",service="artifactapi"`

	w := do(http.MethodGet, "/v2/", "")
	if w.Code != http.StatusUnauthorized || w.Header().Get("WWW-Authenticate") != challenge {
		t.Errorf("anonymous ping: %d %q", w.Code, w.Header().Get("WWW-Authenticate"))
	}
	if w := do(http.MethodGet, "/v2/", "Bearer anonymous"); w.Code != http.StatusNoContent {
		t.Errorf("ping with token: %d", w.Code)
	}
	if w := do(http.MethodGet, "/v2/token", ""); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"token":"anonymous"`) {
		t.Errorf("anonymous token: %d %s", w.Code, w.Body)
	}
	if w := do(http.MethodGet, "/v2/token", basic("ci", valid)); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"token":"`+valid+`"`) {
		t.Errorf("token exchange: %d %s", w.Code, w.Body)
	}
	expired := iss.Token(nil, "artifactapi", "ci", -time.Minute)
	for _, authz := range []string{basic("ci", "bogus"), basic("ci", expired), "Bearer " + expired} {
		if w := do(http.MethodGet, "/v2/token", authz); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"token":"anonymous"`) {
			t.Errorf("rejected credential %q: %d %s", authz, w.Code, w.Body)
		}
	}
	post := func(form url.Values) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, "/v2/token", strings.NewReader(form.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded; charset=utf-8")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	for _, tc := range []struct {
		name string
		form url.Values
		want string
	}{
		{"password grant", url.Values{"grant_type": {"password"}, "username": {"ci"}, "password": {valid}, "service": {"artifactapi"}}, valid},
		{"refresh grant", url.Values{"grant_type": {"refresh_token"}, "refresh_token": {valid}}, valid},
		{"bogus password", url.Values{"grant_type": {"password"}, "username": {"ci"}, "password": {"bogus"}}, "anonymous"},
		{"expired refresh", url.Values{"grant_type": {"refresh_token"}, "refresh_token": {expired}}, "anonymous"},
		{"no credential", url.Values{"grant_type": {"password"}, "username": {"ci"}}, "anonymous"},
	} {
		if w := post(tc.form); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"access_token":"`+tc.want+`"`) {
			t.Errorf("oauth %s: %d %s", tc.name, w.Code, w.Body)
		}
	}
	var mp strings.Builder
	mw := multipart.NewWriter(&mp)
	_ = mw.WriteField("password", valid)
	_ = mw.Close()
	r := httptest.NewRequest(http.MethodPost, "/v2/token", strings.NewReader(mp.String()))
	r.Header.Set("Content-Type", mw.FormDataContentType())
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if !strings.Contains(w.Body.String(), `"token":"anonymous"`) || r.MultipartForm != nil {
		t.Errorf("multipart form honoured: %s", w.Body)
	}
	big := &countingReader{r: io.MultiReader(strings.NewReader("password="+valid+"&pad="), strings.NewReader(strings.Repeat("a", 1<<20)))}
	r = httptest.NewRequest(http.MethodPost, "/v2/token", big)
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if !strings.Contains(w.Body.String(), `"token":"anonymous"`) || big.n > 2*maxTokenForm {
		t.Errorf("oversized form: read %d bytes, %s", big.n, w.Body)
	}
	for _, m := range []string{http.MethodGet, http.MethodHead} {
		if w := do(m, "/v2/repo/img/manifests/t", ""); w.Code != http.StatusNoContent {
			t.Errorf("anonymous %s: %d", m, w.Code)
		}
	}
	for _, m := range []string{http.MethodPost, http.MethodPatch, http.MethodPut, http.MethodDelete} {
		w := do(m, "/v2/repo/img/blobs/uploads/", "Bearer anonymous")
		if w.Code != http.StatusUnauthorized || w.Header().Get("WWW-Authenticate") != challenge {
			t.Errorf("anonymous %s: %d %q", m, w.Code, w.Header().Get("WWW-Authenticate"))
		}
		for _, authz := range []string{"Bearer " + valid, basic("ci", valid)} {
			if w := do(m, "/v2/repo/img/blobs/uploads/", authz); w.Code != http.StatusNoContent {
				t.Errorf("authenticated %s: %d", m, w.Code)
			}
		}
	}
	if got := serve(NewBearerOIDC("", nil).RequireRegistryWrites(ok, nil), http.MethodPut, ""); got != http.StatusNoContent {
		t.Errorf("disabled: status %d", got)
	}
}

// stubRegistry is just enough of a Registry V2 for containerd to push a blob
// and manifest and resolve/fetch them back.
func stubRegistry() http.Handler {
	var mu sync.Mutex
	store := map[string][]byte{}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		p := r.URL.Path
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(p, "/blobs/uploads/"):
			w.Header().Set("Location", p+"u1")
			w.WriteHeader(http.StatusAccepted)
		case r.Method == http.MethodPut && strings.Contains(p, "/blobs/uploads/"):
			dgst := r.URL.Query().Get("digest")
			store[strings.SplitN(p, "/blobs/", 2)[0]+"/blobs/"+dgst], _ = io.ReadAll(r.Body)
			w.Header().Set("Docker-Content-Digest", dgst)
			w.WriteHeader(http.StatusCreated)
		case r.Method == http.MethodPut:
			body, _ := io.ReadAll(r.Body)
			store[p] = body
			store[strings.SplitN(p, "/manifests/", 2)[0]+"/manifests/"+digest.FromBytes(body).String()] = body
			w.Header().Set("Docker-Content-Digest", digest.FromBytes(body).String())
			w.WriteHeader(http.StatusCreated)
		default:
			body, found := store[p]
			if !found {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			if strings.Contains(p, "/manifests/") {
				w.Header().Set("Content-Type", ocispec.MediaTypeImageManifest)
			}
			w.Header().Set("Docker-Content-Digest", digest.FromBytes(body).String())
			w.Header().Set("Content-Length", strconv.Itoa(len(body)))
			if r.Method == http.MethodGet {
				_, _ = w.Write(body)
			}
		}
	})
}

// TestRegistryContainerdClient drives the real containerd resolver (used by
// buildkit), which fetches tokens with an OAuth2 POST form.
func TestRegistryContainerdClient(t *testing.T) {
	iss := testsupport.NewOIDCIssuer()
	defer iss.Close()
	var srv *httptest.Server
	srv = httptest.NewServer(NewBearerOIDC(iss.URL, []string{"artifactapi"}).RequireRegistryWrites(stubRegistry(), func(*http.Request) string { return srv.URL + "/v2/token" }))
	defer srv.Close()
	ref := strings.TrimPrefix(srv.URL, "http://") + "/repo/img:t"
	ctx := context.Background()

	resolver := func(secret string) remotes.Resolver {
		return docker.NewResolver(docker.ResolverOptions{Hosts: docker.ConfigureDefaultRegistries(
			docker.WithPlainHTTP(docker.MatchAllHosts),
			docker.WithAuthorizer(docker.NewDockerAuthorizer(docker.WithAuthCreds(func(string) (string, string, error) { return "ci", secret, nil }))),
		)})
	}
	push := func(res remotes.Resolver, mediaType string, data []byte) (ocispec.Descriptor, error) {
		desc := ocispec.Descriptor{MediaType: mediaType, Digest: digest.FromBytes(data), Size: int64(len(data))}
		p, err := res.Pusher(ctx, ref)
		if err != nil {
			return desc, err
		}
		cw, err := p.Push(ctx, desc)
		if err != nil {
			return desc, err
		}
		defer cw.Close()
		if _, err := cw.Write(data); err != nil {
			return desc, err
		}
		return desc, cw.Commit(ctx, desc.Size, desc.Digest)
	}
	pushImage := func(res remotes.Resolver, arch string) error {
		cfg, err := push(res, ocispec.MediaTypeImageConfig, []byte(`{"architecture":"`+arch+`","os":"linux"}`))
		if err != nil {
			return err
		}
		manifest, _ := json.Marshal(ocispec.Manifest{Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: ocispec.MediaTypeImageManifest, Config: cfg, Layers: []ocispec.Descriptor{}})
		_, err = push(res, ocispec.MediaTypeImageManifest, manifest)
		return err
	}

	if err := pushImage(resolver(iss.Token(nil, "artifactapi", "ci", time.Hour)), "amd64"); err != nil {
		t.Fatalf("push with valid credential: %v", err)
	}
	for name, secret := range map[string]string{"bogus": "bogus", "expired": iss.Token(nil, "artifactapi", "ci", -time.Minute)} {
		if err := pushImage(resolver(secret), name); err == nil || !strings.Contains(err.Error(), "401") {
			t.Errorf("push with %s credential: want 401, got %v", name, err)
		}
		res := resolver(secret)
		_, desc, err := res.Resolve(ctx, ref)
		if err != nil {
			t.Fatalf("resolve with %s credential: %v", name, err)
		}
		f, _ := res.Fetcher(ctx, ref)
		rc, err := f.Fetch(ctx, desc)
		if err != nil {
			t.Fatalf("pull with %s credential: %v", name, err)
		}
		_ = rc.Close()
	}
}

type countingReader struct {
	r io.Reader
	n int
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += n
	return n, err
}
