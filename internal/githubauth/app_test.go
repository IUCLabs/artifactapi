package githubauth

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func testRSAKeyPEM(t *testing.T) string {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	der := x509.MarshalPKCS1PrivateKey(key)
	return string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: der}))
}

// appFixture serves the installation-token exchange endpoint, records requests,
// verifies the presented JWT against the app public key, and returns tokens with
// a controllable expiry.
type appFixture struct {
	srv       *httptest.Server
	pub       *rsa.PublicKey
	mu        sync.Mutex
	exchanges int
	lastJWT   string
	expiresAt func() time.Time
	tokenSeq  int
}

func newAppFixture(t *testing.T, pemKey string) *appFixture {
	t.Helper()
	block, _ := pem.Decode([]byte(pemKey))
	key, err := x509.ParsePKCS1PrivateKey(block.Bytes)
	if err != nil {
		t.Fatalf("parse test key: %v", err)
	}
	f := &appFixture{
		pub:       &key.PublicKey,
		expiresAt: func() time.Time { return time.Now().Add(time.Hour) },
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/app/installations/456/access_tokens", func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		jwt := strings.TrimPrefix(auth, "Bearer ")
		f.mu.Lock()
		f.exchanges++
		f.lastJWT = jwt
		f.tokenSeq++
		seq := f.tokenSeq
		exp := f.expiresAt()
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(map[string]any{
			"token":      fmt.Sprintf("ghs_installation_%d", seq),
			"expires_at": exp.UTC().Format(time.RFC3339),
		})
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *appFixture) verifyJWT(t *testing.T) {
	t.Helper()
	f.mu.Lock()
	jwt := f.lastJWT
	f.mu.Unlock()
	parts := strings.Split(jwt, ".")
	if len(parts) != 3 {
		t.Fatalf("jwt not three-part: %q", jwt)
	}
	signingInput := parts[0] + "." + parts[1]
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		t.Fatalf("decode sig: %v", err)
	}
	digest := sha256.Sum256([]byte(signingInput))
	if err := rsa.VerifyPKCS1v15(f.pub, crypto.SHA256, digest[:], sig); err != nil {
		t.Fatalf("jwt signature invalid: %v", err)
	}
	var claims struct {
		Iss string `json:"iss"`
		Iat int64  `json:"iat"`
		Exp int64  `json:"exp"`
	}
	cb, _ := base64.RawURLEncoding.DecodeString(parts[1])
	if err := json.Unmarshal(cb, &claims); err != nil {
		t.Fatalf("decode claims: %v", err)
	}
	if claims.Iss != "123" {
		t.Fatalf("iss = %q, want 123", claims.Iss)
	}
	if claims.Exp-claims.Iat > int64((10*time.Minute)/time.Second) {
		t.Fatalf("jwt lifetime exceeds 10m: iat=%d exp=%d", claims.Iat, claims.Exp)
	}
	if claims.Iat > time.Now().Unix() {
		t.Fatalf("iat not backdated: %d", claims.Iat)
	}
}

func newAppCred(t *testing.T, f *appFixture, pemKey string) *appCredential {
	t.Helper()
	c, err := newAppCredential(Options{
		AppID:          "123",
		InstallationID: "456",
		PrivateKeyPEM:  pemKey,
		apiBaseURL:     f.srv.URL,
		httpClient:     f.srv.Client(),
	})
	if err != nil {
		t.Fatalf("newAppCredential: %v", err)
	}
	return c
}

func TestApp_MintsJWTAndExchangesForInstallationToken(t *testing.T) {
	pemKey := testRSAKeyPEM(t)
	f := newAppFixture(t, pemKey)
	c := newAppCred(t, f, pemKey)

	tok, err := c.Token(context.Background())
	if err != nil {
		t.Fatalf("token: %v", err)
	}
	if tok != "ghs_installation_1" {
		t.Fatalf("token = %q, want ghs_installation_1", tok)
	}
	if f.exchanges != 1 {
		t.Fatalf("exchanges = %d, want 1", f.exchanges)
	}
	f.verifyJWT(t)
}

func TestApp_CachesInstallationToken(t *testing.T) {
	pemKey := testRSAKeyPEM(t)
	f := newAppFixture(t, pemKey)
	c := newAppCred(t, f, pemKey)

	for i := 0; i < 5; i++ {
		if _, err := c.Token(context.Background()); err != nil {
			t.Fatalf("token: %v", err)
		}
	}
	if f.exchanges != 1 {
		t.Fatalf("exchanges = %d, want 1 (token should be cached)", f.exchanges)
	}
}

func TestApp_RefreshesNearExpiry(t *testing.T) {
	pemKey := testRSAKeyPEM(t)
	f := newAppFixture(t, pemKey)
	// Token expires within refreshSkew, so every call must re-exchange.
	f.expiresAt = func() time.Time { return time.Now().Add(2 * time.Minute) }
	c := newAppCred(t, f, pemKey)

	t1, err := c.Token(context.Background())
	if err != nil {
		t.Fatalf("token 1: %v", err)
	}
	t2, err := c.Token(context.Background())
	if err != nil {
		t.Fatalf("token 2: %v", err)
	}
	if f.exchanges != 2 {
		t.Fatalf("exchanges = %d, want 2 (near-expiry token must refresh)", f.exchanges)
	}
	if t1 == t2 {
		t.Fatalf("expected a fresh token after refresh, both = %q", t1)
	}
}

func TestApp_ConcurrentTokenSingleFlights(t *testing.T) {
	pemKey := testRSAKeyPEM(t)
	f := newAppFixture(t, pemKey)
	c := newAppCred(t, f, pemKey)

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := c.Token(context.Background()); err != nil {
				t.Errorf("token: %v", err)
			}
		}()
	}
	wg.Wait()
	if f.exchanges != 1 {
		t.Fatalf("exchanges = %d, want 1 (concurrent calls must coalesce)", f.exchanges)
	}
}
