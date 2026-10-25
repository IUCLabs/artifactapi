package auth

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"

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
