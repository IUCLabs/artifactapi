//go:build dockere2e

// Package e2edocker holds the black-box end-to-end suite that runs against a
// fully dockerised artifactapi stack (see scripts/docker-e2e.sh). Unlike the
// in-process e2e suite, these tests only speak HTTP to the running container.
package e2edocker

import (
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
)

// authToken is accepted by the stack's write gate; badToken carries the same
// claims signed by a key the issuer never published.
var authToken, badToken string

// TestMain publishes a fresh signing key through the static mockoidc issuer
// (e2e-docker/oidc, served by docker-compose.e2e.yml) and mints both tokens.
func TestMain(m *testing.M) {
	key := mustRSAKey()
	jwks, _ := json.Marshal(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: &key.PublicKey, KeyID: "e2e", Algorithm: "RS256", Use: "sig"}}})
	if err := os.WriteFile(filepath.Join("oidc", "jwks.json"), jwks, 0o644); err != nil {
		log.Fatalf("write jwks: %v", err)
	}
	authToken, badToken = mintToken(key), mintToken(mustRSAKey())
	os.Exit(m.Run())
}

func mustRSAKey() *rsa.PrivateKey {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		log.Fatalf("generate key: %v", err)
	}
	return key
}

func mintToken(key *rsa.PrivateKey) string {
	sig, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: jose.JSONWebKey{Key: key, KeyID: "e2e"}}, (&jose.SignerOptions{}).WithType("JWT"))
	if err != nil {
		log.Fatalf("signer: %v", err)
	}
	now := time.Now()
	tok, err := jwt.Signed(sig).Claims(jwt.Claims{
		Issuer: "http://mockoidc", Subject: "e2e", Audience: jwt.Audience{"artifactapi"},
		IssuedAt: jwt.NewNumericDate(now), Expiry: jwt.NewNumericDate(now.Add(time.Hour)),
	}).Serialize()
	if err != nil {
		log.Fatalf("sign token: %v", err)
	}
	return tok
}

func basicAuth(token string) string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte("ci:"+token))
}

func baseURL() string {
	if v := os.Getenv("ARTIFACTAPI_URL"); v != "" {
		return strings.TrimRight(v, "/")
	}
	return "http://localhost:8000"
}

// mockUpstream is the base URL the artifactapi *container* uses to reach the
// static mock upstream. It is resolved on the compose network, not the host.
func mockUpstream() string {
	if v := os.Getenv("MOCK_UPSTREAM_INTERNAL"); v != "" {
		return strings.TrimRight(v, "/")
	}
	return "http://mockupstream"
}

func api(path string) string { return baseURL() + path }

func fixtureBytes(t *testing.T, rel string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("fixtures", rel))
	if err != nil {
		t.Fatalf("read fixture %s: %v", rel, err)
	}
	return b
}

// doRequest sends authToken as a Bearer token; doRequestAuth sends authz
// verbatim, or no Authorization header when it is empty.
func doRequest(t *testing.T, method, url string, body []byte, contentType string) (*http.Response, []byte) {
	t.Helper()
	return doRequestAuth(t, method, url, body, contentType, "Bearer "+authToken)
}

func doRequestAuth(t *testing.T, method, url string, body []byte, contentType, authz string) (*http.Response, []byte) {
	t.Helper()
	var r io.Reader
	if body != nil {
		r = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, url, r)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if authz != "" {
		req.Header.Set("Authorization", authz)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	return resp, respBody
}

func createRepo(t *testing.T, jsonBody string) {
	t.Helper()
	resp, body := doRequest(t, http.MethodPost, api("/api/v2/remotes"), []byte(jsonBody), "application/json")
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create repo: status %d: %s", resp.StatusCode, body)
	}
}

func deleteRepo(t *testing.T, name string) {
	t.Helper()
	doRequest(t, http.MethodDelete, api("/api/v2/remotes/"+name), nil, "")
}

func createVirtual(t *testing.T, jsonBody string) {
	t.Helper()
	resp, body := doRequest(t, http.MethodPost, api("/api/v2/virtuals"), []byte(jsonBody), "application/json")
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create virtual: status %d: %s", resp.StatusCode, body)
	}
}

func deleteVirtual(t *testing.T, name string) {
	t.Helper()
	doRequest(t, http.MethodDelete, api("/api/v2/virtuals/"+name), nil, "")
}

// getEventually retries a GET until it returns 200 or the deadline passes. Used
// for asynchronously-generated artifacts (e.g. rpm repodata after upload).
func getEventually(t *testing.T, url string, timeout time.Duration) (*http.Response, []byte) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var resp *http.Response
	var body []byte
	for {
		resp, body = doRequest(t, http.MethodGet, url, nil, "")
		if resp.StatusCode == http.StatusOK || time.Now().After(deadline) {
			return resp, body
		}
		time.Sleep(250 * time.Millisecond)
	}
}

// assertWriteGated checks a write is refused with no token and with a token
// from an unknown key, and that readURL still serves anonymously.
func assertWriteGated(t *testing.T, method, url string, body []byte, contentType, readURL string) {
	t.Helper()
	for _, authz := range []string{"", "Bearer " + badToken, basicAuth(badToken)} {
		if resp, b := doRequestAuth(t, method, url, body, contentType, authz); resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s %s with %.12q: status %d, want 401: %s", method, url, authz, resp.StatusCode, b)
		}
	}
	if resp, b := doRequestAuth(t, http.MethodGet, readURL, nil, "", ""); resp.StatusCode != http.StatusOK {
		t.Errorf("anonymous GET %s: status %d: %s", readURL, resp.StatusCode, b)
	}
}
