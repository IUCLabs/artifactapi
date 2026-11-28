// Package githubauthtest installs fake API-managed GitHub Apps for tests.
package githubauthtest

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/IUCLabs/artifactapi/internal/githubauth"
	"github.com/IUCLabs/artifactapi/pkg/models"
)

type store map[string]models.GitHubApp

func (s store) GetGitHubApp(_ context.Context, name string) (*models.GitHubApp, error) {
	a, ok := s[name]
	if !ok {
		return nil, models.ErrGitHubAppNotFound
	}
	return &a, nil
}

func (s store) GitHubAppUpdatedAt(_ context.Context, name string) (time.Time, error) {
	a, ok := s[name]
	if !ok {
		return time.Time{}, models.ErrGitHubAppNotFound
	}
	return a.UpdatedAt, nil
}

// InstallApp installs a process-wide app registry holding one app, name, whose
// installation token exchange returns token. The registry is removed on cleanup.
func InstallApp(t *testing.T, name, token string) {
	t.Helper()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"token": token, "expires_at": time.Now().Add(time.Hour).UTC()})
	}))
	t.Cleanup(srv.Close)
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	keyPEM := string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))
	reg := githubauth.NewApps(store{name: {Name: name, AppID: 1, InstallationID: 2, PrivateKey: keyPEM,
		APIBaseURL: srv.URL, UpdatedAt: time.Now()}}, []string{srv.Listener.Addr().String()})
	reg.SetHTTPClient(srv.Client())
	githubauth.SetApps(reg)
	t.Cleanup(func() { githubauth.SetApps(nil) })
}
