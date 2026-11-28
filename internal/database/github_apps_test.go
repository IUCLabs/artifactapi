package database

import (
	"errors"
	"testing"

	"github.com/IUCLabs/artifactapi/pkg/models"
)

func TestGitHubAppRoundTrip(t *testing.T) {
	requireDB(t)
	app := models.GitHubApp{Name: "rt-app", AppID: 1, InstallationID: 2, APIBaseURL: "https://api.github.com",
		Description: "d", PrivateKeyFingerprint: "SHA256:x", PrivateKey: "PEM"}
	if err := testDB.PutGitHubApp(ctx(), &app); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = testDB.DeleteGitHubApp(ctx(), "rt-app") })
	if app.CreatedAt.IsZero() || !app.CreatedAt.Equal(app.UpdatedAt) {
		t.Fatalf("timestamps on create: %v / %v", app.CreatedAt, app.UpdatedAt)
	}

	got, err := testDB.GetGitHubApp(ctx(), "rt-app")
	if err != nil || got.PrivateKey != "PEM" || got.AppID != 1 || got.InstallationID != 2 || got.Description != "d" {
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
	app := models.GitHubApp{Name: "ref-app", AppID: 1, InstallationID: 2, APIBaseURL: "https://api.github.com", PrivateKey: "PEM"}
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
