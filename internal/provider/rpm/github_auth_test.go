package rpm

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/IUCLabs/artifactapi/internal/githubauth"
	"github.com/IUCLabs/artifactapi/pkg/models"
)

// staticCred is a test Credential yielding a fixed token.
type staticCred string

func (s staticCred) Token(context.Context) (string, error) { return string(s), nil }

func TestGitHubServerCredentialAttachedToReleasesAndAssets(t *testing.T) {
	fx := newGitHubFixture(t, true)
	p := newTestProvider()
	p.serverCred = staticCred("ghp_server_secret")
	store := newFakeStore()

	if err := p.scan(context.Background(), fx.remote(), store); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if got := fx.releaseAuth; got != "Bearer ghp_server_secret" {
		t.Fatalf("releases Authorization = %q, want Bearer ghp_server_secret", got)
	}
	if got := fx.assetAuth; got != "Bearer ghp_server_secret" {
		t.Fatalf("asset Authorization = %q, want Bearer ghp_server_secret", got)
	}
}

func TestGitHubPerRemoteCredentialOverridesServer(t *testing.T) {
	fx := newGitHubFixture(t, true)
	p := newTestProvider()
	p.serverCred = staticCred("ghp_server_secret")
	store := newFakeStore()

	remote := fx.remote()
	remote.Password = "ghp_remote_wins"

	if err := p.scan(context.Background(), remote, store); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if got := fx.releaseAuth; got != "Bearer ghp_remote_wins" {
		t.Fatalf("releases Authorization = %q, want per-remote token to win", got)
	}
	if got := fx.assetAuth; got != "Bearer ghp_remote_wins" {
		t.Fatalf("asset Authorization = %q, want per-remote token to win", got)
	}
}

func TestGitHubNoCredentialSendsNoAuthHeader(t *testing.T) {
	fx := newGitHubFixture(t, true)
	p := newTestProvider() // serverCred nil, package Server() unset in unit tests
	store := newFakeStore()

	if err := p.scan(context.Background(), fx.remote(), store); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if fx.releaseAuth != "" {
		t.Fatalf("expected no Authorization header, got %q", fx.releaseAuth)
	}
	if fx.assetAuth != "" {
		t.Fatalf("expected no asset Authorization header, got %q", fx.assetAuth)
	}
	// Requests still succeed anonymously.
	if rows, _ := store.ListRPMMetadataEntries(context.Background(), "acme-rpm"); len(rows) != 1 {
		t.Fatalf("anonymous scan should still derive metadata, got %d rows", len(rows))
	}
}

func TestGitHubETag304FlowWithAuth(t *testing.T) {
	fx := newGitHubFixture(t, true)
	fx.etag = `"v1"`
	p := newTestProvider()
	p.serverCred = staticCred("ghp_server_secret")
	store := newFakeStore()

	etag, changed, err := p.scanWithState(context.Background(), fx.remote(), store, "")
	if err != nil {
		t.Fatalf("first scan: %v", err)
	}
	if !changed || etag != `"v1"` {
		t.Fatalf("first scan changed=%v etag=%q, want true and \"v1\"", changed, etag)
	}

	// Re-scan with the captured ETag: a 304 means no change and no asset fetch.
	etag2, changed2, err := p.scanWithState(context.Background(), fx.remote(), store, etag)
	if err != nil {
		t.Fatalf("second scan: %v", err)
	}
	if changed2 {
		t.Fatal("expected no change on 304")
	}
	if etag2 != `"v1"` {
		t.Fatalf("etag = %q, want preserved \"v1\"", etag2)
	}
	if fx.notModHit != 1 {
		t.Fatalf("expected exactly one 304 response, got %d", fx.notModHit)
	}
	// The conditional request still carried the credential.
	if fx.releaseAuth != "Bearer ghp_server_secret" {
		t.Fatalf("conditional request Authorization = %q, want the server credential", fx.releaseAuth)
	}
}

// TestGitHubCredentialAbsentFromRemoteJSON asserts the server credential never
// appears in a remote's serialized API representation, and per-remote secrets
// stay redacted by the models.Remote json:"-" tags.
func TestGitHubCredentialAbsentFromRemoteJSON(t *testing.T) {
	githubauth.SetServer(staticCred("ghp_super_secret_server_token"))
	t.Cleanup(func() { githubauth.SetServer(nil) })

	remote := models.Remote{
		Name:        "acme-rpm",
		PackageType: models.PackageGitHubRPM,
		BaseURL:     "https://api.github.com/repos/acme/tools",
		Username:    "per_remote_user",
		Password:    "per_remote_secret",
	}
	b, err := json.Marshal(remote)
	if err != nil {
		t.Fatalf("marshal remote: %v", err)
	}
	js := string(b)
	for _, secret := range []string{"ghp_super_secret_server_token", "per_remote_secret", "per_remote_user"} {
		if strings.Contains(js, secret) {
			t.Fatalf("credential %q leaked into remote JSON: %s", secret, js)
		}
	}
}
