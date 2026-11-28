package database

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/IUCLabs/artifactapi/internal/githubauth"
	"github.com/IUCLabs/artifactapi/pkg/models"
)

func testAppKeyPEM(t *testing.T) string {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))
}

func TestGitHubAppRoundTrip(t *testing.T) {
	requireDB(t)
	keyPEM := testAppKeyPEM(t)
	key, err := githubauth.ParsePrivateKey(keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	wantFP, _ := githubauth.Fingerprint(key)
	app := models.GitHubApp{Name: "rt-app", AppID: 1, InstallationID: 2, APIBaseURL: "https://api.github.com",
		Description: "d", PrivateKeyFingerprint: "SHA256:caller-supplied", PrivateKey: keyPEM}
	if err := testDB.PutGitHubApp(ctx(), &app); err != nil {
		t.Fatal(err)
	}
	if app.PrivateKeyFingerprint != wantFP {
		t.Fatalf("fingerprint = %q; want %q derived from the key", app.PrivateKeyFingerprint, wantFP)
	}
	if err := testDB.PutGitHubApp(ctx(), &models.GitHubApp{Name: "rt-bad", AppID: 1, InstallationID: 2, PrivateKey: "junk"}); err == nil {
		t.Fatal("unparseable key stored")
	}
	t.Cleanup(func() { _ = testDB.DeleteGitHubApp(ctx(), "rt-app") })
	if app.CreatedAt.IsZero() || !app.CreatedAt.Equal(app.UpdatedAt) {
		t.Fatalf("timestamps on create: %v / %v", app.CreatedAt, app.UpdatedAt)
	}

	if ua, err := testDB.GitHubAppUpdatedAt(ctx(), "rt-app"); err != nil || !ua.Equal(app.UpdatedAt) {
		t.Fatalf("updated_at = %v, %v; want %v", ua, err, app.UpdatedAt)
	}
	if _, err := testDB.GitHubAppUpdatedAt(ctx(), "rt-missing"); !errors.Is(err, ErrGitHubAppNotFound) {
		t.Fatalf("updated_at of missing app: %v", err)
	}

	got, err := testDB.GetGitHubApp(ctx(), "rt-app")
	if err != nil || got.PrivateKey != keyPEM || got.PrivateKeyFingerprint != wantFP || got.AppID != 1 || got.InstallationID != 2 || got.Description != "d" {
		t.Fatalf("get: %+v, %v", got, err)
	}

	same := app
	if err := testDB.PutGitHubApp(ctx(), &same); err != nil {
		t.Fatal(err)
	}
	if !same.UpdatedAt.Equal(app.UpdatedAt) {
		t.Fatalf("identical put moved updated_at %v -> %v", app.UpdatedAt, same.UpdatedAt)
	}

	changed := app
	changed.InstallationID = 3
	if err := testDB.PutGitHubApp(ctx(), &changed); err != nil {
		t.Fatal(err)
	}
	if !changed.UpdatedAt.After(app.UpdatedAt) || !changed.CreatedAt.Equal(app.CreatedAt) {
		t.Fatalf("replace timestamps: created %v updated %v", changed.CreatedAt, changed.UpdatedAt)
	}

	list, err := testDB.ListGitHubApps(ctx())
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, a := range list {
		if a.Name == "rt-app" {
			found = true
			if a.PrivateKey != "" || a.InstallationID != 3 {
				t.Fatalf("list row: %+v", a)
			}
		}
	}
	if !found {
		t.Fatal("list missing rt-app")
	}
}

func TestGitHubAppReferencedByRemote(t *testing.T) {
	requireDB(t)
	app := models.GitHubApp{Name: "ref-app", AppID: 1, InstallationID: 2, APIBaseURL: "https://api.github.com", PrivateKey: testAppKeyPEM(t)}
	if err := testDB.PutGitHubApp(ctx(), &app); err != nil {
		t.Fatal(err)
	}
	r := models.Remote{Name: "ref-remote", PackageType: models.PackageGeneric, RepoType: models.RepoTypeRemote,
		BaseURL: "https://github.com", GitHubApp: "ref-app"}
	if err := testDB.CreateRemote(ctx(), &r); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = testDB.DeleteRemote(ctx(), "ref-remote"); _ = testDB.DeleteGitHubApp(ctx(), "ref-app") })

	got, err := testDB.GetRemote(ctx(), "ref-remote")
	if err != nil || got.GitHubApp != "ref-app" {
		t.Fatalf("remote github_app = %+v, %v", got, err)
	}

	var inUse *GitHubAppInUseError
	if err := testDB.DeleteGitHubApp(ctx(), "ref-app"); !errors.As(err, &inUse) || len(inUse.Remotes) != 1 || inUse.Remotes[0] != "ref-remote" {
		t.Fatalf("delete referenced app: %v", err)
	}

	got.GitHubApp = ""
	if err := testDB.UpdateRemote(ctx(), got); err != nil {
		t.Fatal(err)
	}
	if got, _ := testDB.GetRemote(ctx(), "ref-remote"); got.GitHubApp != "" {
		t.Fatalf("cleared github_app = %q", got.GitHubApp)
	}
	if err := testDB.DeleteGitHubApp(ctx(), "ref-app"); err != nil {
		t.Fatalf("delete unreferenced app: %v", err)
	}
	if err := testDB.DeleteGitHubApp(ctx(), "ref-app"); !errors.Is(err, ErrGitHubAppNotFound) {
		t.Fatalf("delete missing app: %v", err)
	}

	got.GitHubApp = "ref-app"
	if err := testDB.UpdateRemote(ctx(), got); !IsForeignKeyViolation(err) {
		t.Fatalf("referencing a missing app: %v, want fk violation", err)
	}
}

func TestCreateGitHubAppUninstalled(t *testing.T) {
	requireDB(t)
	keyPEM := testAppKeyPEM(t)
	app := models.GitHubApp{Name: "manifest-app", AppID: 7, APIBaseURL: "https://api.github.com", PrivateKey: keyPEM}
	if err := testDB.CreateGitHubApp(ctx(), &app); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = testDB.DeleteGitHubApp(ctx(), "manifest-app") })
	if !strings.HasPrefix(app.PrivateKeyFingerprint, "SHA256:") || app.CreatedAt.IsZero() {
		t.Fatalf("created = %+v", app)
	}
	dup := models.GitHubApp{Name: "manifest-app", AppID: 8, APIBaseURL: "https://api.github.com", PrivateKey: testAppKeyPEM(t)}
	if err := testDB.CreateGitHubApp(ctx(), &dup); !errors.Is(err, ErrGitHubAppExists) {
		t.Fatalf("duplicate create err = %v, want ErrGitHubAppExists", err)
	}
	got, err := testDB.GetGitHubApp(ctx(), "manifest-app")
	if err != nil || got.InstallationID != 0 || got.AppID != 7 || got.PrivateKey != keyPEM {
		t.Fatalf("get = %+v %v", got, err)
	}
	listed, err := testDB.ListGitHubApps(ctx())
	if err != nil || !slices.ContainsFunc(listed, func(a models.GitHubApp) bool { return a.Name == "manifest-app" && a.InstallationID == 0 }) {
		t.Fatalf("list = %+v %v", listed, err)
	}

	got.InstallationID = 1 << 40
	before := got.UpdatedAt
	if err := testDB.PutGitHubApp(ctx(), got); err != nil {
		t.Fatal(err)
	}
	if !got.UpdatedAt.After(before) {
		t.Fatalf("installing did not bump updated_at: %v -> %v", before, got.UpdatedAt)
	}
	if again, _ := testDB.GetGitHubApp(ctx(), "manifest-app"); again.InstallationID != 1<<40 {
		t.Fatalf("installation_id = %d", again.InstallationID)
	}
}
