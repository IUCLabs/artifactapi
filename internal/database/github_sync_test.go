package database

import (
	"testing"
	"time"

	"github.com/IUCLabs/artifactapi/pkg/models"
)

func seedGitHubRPMRemote(t *testing.T, name string) {
	t.Helper()
	if err := testDB.CreateRemote(ctx(), &models.Remote{
		Name: name, PackageType: models.PackageGitHubRPM, RepoType: models.RepoTypeRemote,
		BaseURL: "https://api.github.com/repos/acme/tools", ReleasesRemote: "github", MutableTTL: 3600,
	}); err != nil {
		t.Fatalf("seed github_rpm remote: %v", err)
	}
}

// TestGitHubSyncLease exercises the real SQL: exactly one replica may hold the
// lease, the recency window blocks a too-soon periodic re-claim, and a prime
// (freshness 0) bypasses recency but still respects a live lease.
func TestGitHubSyncLease(t *testing.T) {
	requireDB(t)
	name := "gh-lease-" + time.Now().Format("150405.000000")
	seedGitHubRPMRemote(t, name)

	const lease = 15 * time.Minute
	freshness := time.Hour

	// First claim on a never-synced remote wins; etag starts empty.
	claimed, etag, err := testDB.ClaimGitHubSyncLease(ctx(), name, "replica-1", freshness, lease)
	if err != nil || !claimed {
		t.Fatalf("replica-1 first claim: claimed=%v err=%v", claimed, err)
	}
	if etag != "" {
		t.Fatalf("initial etag should be empty, got %q", etag)
	}

	// A second replica cannot claim while the lease is held.
	claimed2, _, err := testDB.ClaimGitHubSyncLease(ctx(), name, "replica-2", freshness, lease)
	if err != nil {
		t.Fatalf("replica-2 claim err: %v", err)
	}
	if claimed2 {
		t.Fatal("replica-2 claimed while replica-1 holds the lease")
	}

	// Replica 1 finishes: record the sync and persist an etag.
	if err := testDB.ReleaseGitHubSyncLease(ctx(), name, "replica-1", `"etag-1"`, time.Now()); err != nil {
		t.Fatalf("release: %v", err)
	}

	// A periodic re-claim inside the freshness window is blocked by recency.
	claimed3, _, err := testDB.ClaimGitHubSyncLease(ctx(), name, "replica-2", freshness, lease)
	if err != nil {
		t.Fatalf("replica-2 recency claim err: %v", err)
	}
	if claimed3 {
		t.Fatal("periodic claim succeeded inside the freshness window")
	}

	// A prime (freshness 0) bypasses recency and reads the persisted etag.
	claimed4, etag4, err := testDB.ClaimGitHubSyncLease(ctx(), name, "replica-2", 0, lease)
	if err != nil || !claimed4 {
		t.Fatalf("prime claim: claimed=%v err=%v", claimed4, err)
	}
	if etag4 != `"etag-1"` {
		t.Fatalf("prime claim etag = %q, want persisted \"etag-1\"", etag4)
	}
}

func TestListGitHubRPMRemotes(t *testing.T) {
	requireDB(t)
	name := "gh-list-" + time.Now().Format("150405.000000")
	seedGitHubRPMRemote(t, name)
	seedRemote(t, "generic-"+time.Now().Format("150405.000000"))

	remotes, err := testDB.ListGitHubRPMRemotes(ctx())
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	found := false
	for _, r := range remotes {
		if r.PackageType != models.PackageGitHubRPM {
			t.Fatalf("non-github_rpm remote returned: %s (%s)", r.Name, r.PackageType)
		}
		if r.Name == name {
			found = true
		}
	}
	if !found {
		t.Fatalf("seeded remote %q not returned", name)
	}
}
