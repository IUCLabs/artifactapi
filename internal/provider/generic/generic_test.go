package generic_test

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/IUCLabs/artifactapi/internal/githubauth"
	"github.com/IUCLabs/artifactapi/internal/provider"
	"github.com/IUCLabs/artifactapi/internal/provider/generic"
	"github.com/IUCLabs/artifactapi/pkg/models"
)

type staticCred string

func (s staticCred) Token(context.Context) (string, error) { return string(s), nil }

func TestProvider_AuthHeaders_GitHubServerCredential(t *testing.T) {
	githubauth.SetServer(staticCred("ghs_server"))
	t.Cleanup(func() { githubauth.SetServer(nil) })

	p := &generic.Provider{}
	h, err := p.AuthHeaders(context.Background(), models.Remote{BaseURL: "https://github.com"})
	if err != nil {
		t.Fatalf("auth headers: %v", err)
	}
	if h.Get("Authorization") != "Bearer ghs_server" {
		t.Fatalf("Authorization = %q, want Bearer ghs_server", h.Get("Authorization"))
	}
}

func TestProvider_AuthHeaders_NonGitHubHostNoServerCredential(t *testing.T) {
	githubauth.SetServer(staticCred("ghs_server"))
	t.Cleanup(func() { githubauth.SetServer(nil) })

	p := &generic.Provider{}
	h, _ := p.AuthHeaders(context.Background(), models.Remote{BaseURL: "https://example.com/downloads"})
	if h.Get("Authorization") != "" {
		t.Fatalf("server credential must not be sent to non-github host, got %q", h.Get("Authorization"))
	}
}

func TestProvider_AuthHeaders_PerRemoteOverridesServerCredential(t *testing.T) {
	githubauth.SetServer(staticCred("ghs_server"))
	t.Cleanup(func() { githubauth.SetServer(nil) })

	p := &generic.Provider{}
	h, _ := p.AuthHeaders(context.Background(), models.Remote{
		BaseURL:  "https://github.com",
		Username: "user",
		Password: "pass",
	})
	if got := h.Get("Authorization"); got != "Basic dXNlcjpwYXNz" {
		t.Fatalf("per-remote Basic auth must win, got %q", got)
	}
}

func TestProvider_Type(t *testing.T) {
	p := &generic.Provider{}
	if p.Type() != models.PackageGeneric {
		t.Errorf("expected generic, got %q", p.Type())
	}
}

func TestProvider_Classify_AllImmutable(t *testing.T) {
	p := &generic.Provider{}
	paths := []string{"file.tar.gz", "path/to/binary", "index.html", "data.json"}
	for _, path := range paths {
		if p.Classify(path) != provider.Immutable {
			t.Errorf("generic should classify %q as immutable", path)
		}
	}
}

func TestProvider_ContentType(t *testing.T) {
	p := &generic.Provider{}
	tests := []struct{ path, want string }{
		{"file.tar.gz", "application/gzip"},
		{"file.tgz", "application/gzip"},
		{"file.zip", "application/zip"},
		{"file.rpm", "application/x-rpm"},
		{"file.json", "application/json"},
		{"file.unknown", "application/octet-stream"},
	}
	for _, tt := range tests {
		if got := p.ContentType(tt.path); got != tt.want {
			t.Errorf("ContentType(%q) = %q, want %q", tt.path, got, tt.want)
		}
	}
}

func TestProvider_UpstreamURL(t *testing.T) {
	p := &generic.Provider{}
	got := p.UpstreamURL(models.Remote{BaseURL: "https://example.com/repo"}, "path/to/file.tar.gz")
	want := "https://example.com/repo/path/to/file.tar.gz"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestProvider_AuthHeaders_BasicAuth(t *testing.T) {
	p := &generic.Provider{}
	h, _ := p.AuthHeaders(context.Background(), models.Remote{Username: "user", Password: "pass"})
	if h.Get("Authorization") != "Basic dXNlcjpwYXNz" {
		t.Errorf("unexpected auth header: %q", h.Get("Authorization"))
	}
}

func TestProvider_AuthHeaders_NoAuth(t *testing.T) {
	p := &generic.Provider{}
	h, _ := p.AuthHeaders(context.Background(), models.Remote{})
	if h.Get("Authorization") != "" {
		t.Error("expected no auth header")
	}
}

type appStore map[string]models.GitHubApp

func (s appStore) GetGitHubApp(_ context.Context, name string) (*models.GitHubApp, error) {
	a, ok := s[name]
	if !ok {
		return nil, errors.New("not found")
	}
	return &a, nil
}

func TestProvider_AuthHeaders_GitHubAppWins(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"token":"ghs_app","expires_at":"2099-01-01T00:00:00Z"}`))
	}))
	t.Cleanup(srv.Close)
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	keyPEM := string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))
	githubauth.SetServer(staticCred("ghs_server"))
	githubauth.SetApps(githubauth.NewApps(appStore{"ci": {Name: "ci", AppID: 1, InstallationID: 2, PrivateKey: keyPEM, APIBaseURL: srv.URL}}))
	t.Cleanup(func() { githubauth.SetServer(nil); githubauth.SetApps(nil) })

	p := &generic.Provider{}
	h, err := p.AuthHeaders(context.Background(), models.Remote{BaseURL: "https://github.com", GitHubApp: "ci"})
	if err != nil || h.Get("Authorization") != "Bearer ghs_app" {
		t.Fatalf("Authorization = %q, %v; want Bearer ghs_app", h.Get("Authorization"), err)
	}
	if _, err := p.AuthHeaders(context.Background(), models.Remote{BaseURL: "https://github.com", GitHubApp: "gone"}); err == nil {
		t.Fatal("missing github_app must fail rather than fall back to the server credential")
	}
}
