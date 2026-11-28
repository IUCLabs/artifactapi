// Package keycrypttest provides a fake Vault (Kubernetes auth + transit).
package keycrypttest

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/IUCLabs/artifactapi/internal/keycrypt"
)

// Role and Key are what the fake accepts.
const (
	Role = "artifactapi"
	Key  = "github-apps"
)

// Vault is an httptest Vault with a derived transit key. Ciphertext is
// "vault:v1:" + base64(context ":" base64(reversed plaintext)), so it never
// contains the plaintext and only decrypts under the same context.
type Vault struct {
	*httptest.Server
	TokenPath string

	mu          sync.Mutex
	Lease       int64
	Renewable   bool
	EchoErrors  bool
	FailEncrypt bool
	DenyTransit bool
	tokens      map[string]bool
	n           int
	JWTs        []string
	Renews      int
	Transit     int
}

// NewVault starts the fake and writes a service account token file "jwt-1".
func NewVault(t *testing.T) *Vault {
	t.Helper()
	v := &Vault{Lease: 3600, Renewable: true, tokens: map[string]bool{}, TokenPath: filepath.Join(t.TempDir(), "token")}
	v.SetJWT(t, "jwt-1")
	v.Server = httptest.NewServer(http.HandlerFunc(v.serve))
	t.Cleanup(v.Close)
	return v
}

// Config returns a transit config pointing at the fake.
func (v *Vault) Config() keycrypt.VaultConfig {
	return keycrypt.VaultConfig{
		Addr: v.URL, Role: Role, K8sMount: keycrypt.DefaultK8sMount, TokenPath: v.TokenPath,
		TransitMount: keycrypt.DefaultTransitMount, TransitKey: Key, AllowInsecureHTTP: true,
	}
}

// SetJWT rewrites the projected service account token.
func (v *Vault) SetJWT(t *testing.T, jwt string) {
	t.Helper()
	if err := os.WriteFile(v.TokenPath, []byte(jwt+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// Revoke invalidates every issued token.
func (v *Vault) Revoke() {
	v.mu.Lock()
	clear(v.tokens)
	v.mu.Unlock()
}

// Logins returns the JWTs presented to each login so far.
func (v *Vault) Logins() []string {
	v.mu.Lock()
	defer v.mu.Unlock()
	return slices.Clone(v.JWTs)
}

// Stats returns renew-self and transit call counts.
func (v *Vault) Stats() (renews, transit int) {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.Renews, v.Transit
}

// Set mutates fake state under its lock.
func (v *Vault) Set(f func(v *Vault)) {
	v.mu.Lock()
	f(v)
	v.mu.Unlock()
}

func vaultError(w http.ResponseWriter, status int, msg string) {
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string][]string{"errors": {msg}})
}

func (v *Vault) authResponse(w http.ResponseWriter, token string) {
	_ = json.NewEncoder(w).Encode(map[string]any{"auth": map[string]any{
		"client_token": token, "lease_duration": v.Lease, "renewable": v.Renewable,
	}})
}

func reverse(b []byte) []byte {
	out := slices.Clone(b)
	slices.Reverse(out)
	return out
}

func (v *Vault) serve(w http.ResponseWriter, r *http.Request) {
	v.mu.Lock()
	defer v.mu.Unlock()
	var body map[string]string
	if r.Method != http.MethodPost || json.NewDecoder(r.Body).Decode(&body) != nil {
		vaultError(w, http.StatusBadRequest, "bad request")
		return
	}
	token := r.Header.Get("X-Vault-Token")
	switch r.URL.Path {
	case "/v1/auth/kubernetes/login":
		v.JWTs = append(v.JWTs, body["jwt"])
		if body["role"] != Role {
			vaultError(w, http.StatusBadRequest, "invalid role name")
			return
		}
		v.n++
		tok := fmt.Sprintf("hvs.%d", v.n)
		v.tokens[tok] = true
		v.authResponse(w, tok)
		return
	case "/v1/auth/token/renew-self":
		if !v.tokens[token] {
			vaultError(w, http.StatusForbidden, "permission denied")
			return
		}
		v.Renews++
		v.authResponse(w, token)
		return
	}
	if !v.tokens[token] || v.DenyTransit {
		vaultError(w, http.StatusForbidden, "permission denied")
		return
	}
	v.Transit++
	switch r.URL.Path {
	case "/v1/transit/encrypt/" + Key:
		if body["context"] == "" {
			vaultError(w, http.StatusBadRequest, "missing 'context' for key derivation")
			return
		}
		pt, err := base64.StdEncoding.DecodeString(body["plaintext"])
		if err != nil || v.FailEncrypt {
			msg := "encryption failed"
			if v.EchoErrors {
				msg += ": " + string(pt)
			}
			vaultError(w, http.StatusInternalServerError, msg)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]string{
			"ciphertext": "vault:v1:" + base64.StdEncoding.EncodeToString(
				[]byte(body["context"]+":"+base64.StdEncoding.EncodeToString(reverse(pt)))),
		}})
	case "/v1/transit/decrypt/" + Key:
		raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(body["ciphertext"], "vault:v1:"))
		dctx, sealed, ok := strings.Cut(string(raw), ":")
		if err != nil || !ok || !strings.HasPrefix(body["ciphertext"], "vault:v1:") {
			vaultError(w, http.StatusBadRequest, "invalid ciphertext")
			return
		}
		pt, err := base64.StdEncoding.DecodeString(sealed)
		if err != nil || dctx != body["context"] {
			vaultError(w, http.StatusBadRequest, "cipher: message authentication failed")
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]string{
			"plaintext": base64.StdEncoding.EncodeToString(reverse(pt)),
		}})
	default:
		vaultError(w, http.StatusNotFound, "no handler")
	}
}
