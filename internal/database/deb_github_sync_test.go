package database

import (
	"testing"
	"time"

	"github.com/IUCLabs/artifactapi/internal/provider"
	"github.com/IUCLabs/artifactapi/pkg/models"
)

func seedGitHubDebRemote(t *testing.T, name string) {
	t.Helper()
	if err := testDB.CreateRemote(ctx(), &models.Remote{
		Name: name, PackageType: models.PackageGitHubDeb, RepoType: models.RepoTypeRemote,
		BaseURL: "https://api.github.com/repos/acme/tools", ReleasesRemote: "github", MutableTTL: 3600,
	}); err != nil {
		t.Fatalf("seed github_deb remote: %v", err)
	}
}

// TestGitHubDebSyncLease exercises the real SQL: exactly one replica may hold the
// lease, the recency window blocks a too-soon periodic re-claim, and a prime
// (freshness 0) bypasses recency but still respects a live lease.
func TestGitHubDebSyncLease(t *testing.T) {
	requireDB(t)
	name := "ghdeb-lease-" + time.Now().Format("150405.000000")
	seedGitHubDebRemote(t, name)

	const lease = 15 * time.Minute
	freshness := time.Hour

	claimed, etag, err := testDB.ClaimGitHubDebSyncLease(ctx(), name, "replica-1", freshness, lease)
	if err != nil || !claimed {
		t.Fatalf("replica-1 first claim: claimed=%v err=%v", claimed, err)
	}
	if etag != "" {
		t.Fatalf("initial etag should be empty, got %q", etag)
	}

	claimed2, _, err := testDB.ClaimGitHubDebSyncLease(ctx(), name, "replica-2", freshness, lease)
	if err != nil {
		t.Fatalf("replica-2 claim err: %v", err)
	}
	if claimed2 {
		t.Fatal("replica-2 claimed while replica-1 holds the lease")
	}

	if err := testDB.ReleaseGitHubDebSyncLease(ctx(), name, "replica-1", provider.SyncResult{Etag: `"etag-1"`}); err != nil {
		t.Fatalf("release: %v", err)
	}

	claimed3, _, err := testDB.ClaimGitHubDebSyncLease(ctx(), name, "replica-2", freshness, lease)
	if err != nil {
		t.Fatalf("replica-2 recency claim err: %v", err)
	}
	if claimed3 {
		t.Fatal("periodic claim succeeded inside the freshness window")
	}

	claimed4, etag4, err := testDB.ClaimGitHubDebSyncLease(ctx(), name, "replica-2", 0, lease)
	if err != nil || !claimed4 {
		t.Fatalf("prime claim: claimed=%v err=%v", claimed4, err)
	}
	if etag4 != `"etag-1"` {
		t.Fatalf("prime claim etag = %q, want persisted \"etag-1\"", etag4)
	}
}

func TestListGitHubDebRemotes(t *testing.T) {
	requireDB(t)
	name := "ghdeb-list-" + time.Now().Format("150405.000000")
	seedGitHubDebRemote(t, name)
	seedRemote(t, "generic-"+time.Now().Format("150405.000000"))

	remotes, err := testDB.ListGitHubDebRemotes(ctx())
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	found := false
	for _, r := range remotes {
		if r.PackageType != models.PackageGitHubDeb {
			t.Fatalf("non-github_deb remote returned: %s (%s)", r.Name, r.PackageType)
		}
		if r.Name == name {
			found = true
		}
	}
	if !found {
		t.Fatalf("seeded remote %q not returned", name)
	}
}
