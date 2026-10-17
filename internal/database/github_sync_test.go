package database

import (
	"testing"
	"time"

	"github.com/IUCLabs/artifactapi/internal/provider"
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
	if err := testDB.ReleaseGitHubSyncLease(ctx(), name, "replica-1", provider.SyncResult{Etag: `"etag-1"`}); err != nil {
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

// A failed scan must not count as a sync: the etag and last_synced_at stay put,
// every claim (prime included) is held off until the backoff elapses, the delay
// doubles per consecutive failure up to the cap, an upstream retry hint pushes
// it later, and a success clears the retry state.
func TestGitHubSyncLeaseFailureBackoff(t *testing.T) {
	requireDB(t)
	name := "gh-retry-" + time.Now().Format("150405.000000")
	seedGitHubRPMRemote(t, name)

	const lease = 15 * time.Minute
	freshness := time.Hour
	claim := func(f time.Duration) (bool, string) {
		t.Helper()
		ok, etag, err := testDB.ClaimGitHubSyncLease(ctx(), name, "r1", f, lease)
		if err != nil {
			t.Fatalf("claim: %v", err)
		}
		return ok, etag
	}
	release := func(res provider.SyncResult) {
		t.Helper()
		if err := testDB.ReleaseGitHubSyncLease(ctx(), name, "r1", res); err != nil {
			t.Fatalf("release: %v", err)
		}
	}
	state := func() (failures int, retryIn time.Duration, synced *time.Time, etag string) {
		t.Helper()
		var next *time.Time
		var dbNow time.Time
		if err := testDB.Pool.QueryRow(ctx(), `SELECT sync_failures, next_retry_at, last_synced_at, etag, now() FROM github_rpm_sync_state WHERE remote_name = $1`, name).
			Scan(&failures, &next, &synced, &etag, &dbNow); err != nil {
			t.Fatalf("read state: %v", err)
		}
		if next != nil {
			retryIn = next.Sub(dbNow)
		}
		return
	}
	elapse := func() {
		t.Helper()
		if _, err := testDB.Pool.Exec(ctx(), `UPDATE github_rpm_sync_state SET next_retry_at = now() - interval '1 second' WHERE remote_name = $1`, name); err != nil {
			t.Fatalf("elapse backoff: %v", err)
		}
	}
	failed := provider.SyncResult{Failed: true, Etag: `"ignored"`, Backoff: time.Minute, MaxBackoff: 3 * time.Minute}

	// Last good sync.
	if ok, _ := claim(freshness); !ok {
		t.Fatal("first claim failed")
	}
	release(provider.SyncResult{Etag: `"good"`})
	_, _, goodSynced, _ := state()

	// First failure: 60s backoff, sync time and etag untouched.
	if ok, _ := claim(0); !ok {
		t.Fatal("prime claim failed")
	}
	release(failed)
	n, in, synced, etag := state()
	if n != 1 || in < 55*time.Second || in > 65*time.Second {
		t.Fatalf("after 1st failure: failures=%d retry in %v, want 1 and ~60s", n, in)
	}
	if etag != `"good"` || synced == nil || !synced.Equal(*goodSynced) {
		t.Fatalf("failed scan overwrote sync state: etag=%q synced=%v", etag, synced)
	}
	if ok, _ := claim(0); ok {
		t.Fatal("prime claimed inside the retry backoff")
	}

	// Backoff elapsed: claimable despite being inside the freshness window,
	// and the second failure doubles the delay.
	elapse()
	if ok, _ := claim(freshness); !ok {
		t.Fatal("periodic claim refused after backoff elapsed")
	}
	release(failed)
	if n, in, _, _ := state(); n != 2 || in < 115*time.Second || in > 125*time.Second {
		t.Fatalf("after 2nd failure: failures=%d retry in %v, want 2 and ~120s", n, in)
	}

	// Third failure caps at MaxBackoff (3m, not 4m).
	elapse()
	claim(freshness)
	release(failed)
	if n, in, _, _ := state(); n != 3 || in < 175*time.Second || in > 185*time.Second {
		t.Fatalf("after 3rd failure: failures=%d retry in %v, want 3 and ~180s", n, in)
	}

	// An upstream rate-limit reset later than the backoff wins.
	elapse()
	claim(freshness)
	hinted := failed
	hinted.RetryAt = time.Now().Add(20 * time.Minute)
	release(hinted)
	if _, in, _, _ := state(); in < 19*time.Minute || in > 21*time.Minute {
		t.Fatalf("retry hint ignored: retry in %v, want ~20m", in)
	}

	// Recovery: success persists the new etag and clears the retry, so the
	// normal freshness window applies again.
	elapse()
	if ok, etag := claim(freshness); !ok || etag != `"good"` {
		t.Fatalf("recovery claim ok=%v etag=%q, want last good etag", ok, etag)
	}
	release(provider.SyncResult{Etag: `"new"`})
	if n, in, _, etag := state(); n != 0 || in != 0 || etag != `"new"` {
		t.Fatalf("after success: failures=%d retry in %v etag=%q", n, in, etag)
	}
	if ok, _ := claim(freshness); ok {
		t.Fatal("claimed inside freshness window after a successful sync")
	}
}

// Deleting a remote drops its sync state, so a recreated remote of the same name
// starts clean instead of inheriting the old ETag, sync time and retry state.
func TestGitHubSyncStateDeletedWithRemote(t *testing.T) {
	requireDB(t)
	for _, tc := range []struct {
		table   string
		pt      models.PackageType
		claim   func(name string) (bool, string, error)
		release func(name string, res provider.SyncResult) error
	}{
		{"github_rpm_sync_state", models.PackageGitHubRPM,
			func(n string) (bool, string, error) {
				return testDB.ClaimGitHubSyncLease(ctx(), n, "r1", time.Hour, time.Minute)
			},
			func(n string, res provider.SyncResult) error {
				return testDB.ReleaseGitHubSyncLease(ctx(), n, "r1", res)
			}},
		{"github_deb_sync_state", models.PackageGitHubDeb,
			func(n string) (bool, string, error) {
				return testDB.ClaimGitHubDebSyncLease(ctx(), n, "r1", time.Hour, time.Minute)
			},
			func(n string, res provider.SyncResult) error {
				return testDB.ReleaseGitHubDebSyncLease(ctx(), n, "r1", res)
			}},
		{"github_alpine_sync_state", models.PackageGitHubAlpine,
			func(n string) (bool, string, error) {
				return testDB.ClaimGitHubAlpineSyncLease(ctx(), n, "r1", time.Hour, time.Minute)
			},
			func(n string, res provider.SyncResult) error {
				return testDB.ReleaseGitHubAlpineSyncLease(ctx(), n, "r1", res)
			}},
	} {
		t.Run(tc.table, func(t *testing.T) {
			name := "gh-recreate-" + time.Now().Format("150405.000000")
			create := func() {
				t.Helper()
				if err := testDB.CreateRemote(ctx(), &models.Remote{
					Name: name, PackageType: tc.pt, RepoType: models.RepoTypeRemote,
					BaseURL: "https://api.github.com/repos/acme/tools", ReleasesRemote: "github", MutableTTL: 3600,
				}); err != nil {
					t.Fatalf("create remote: %v", err)
				}
			}

			create()
			if ok, _, err := tc.claim(name); err != nil || !ok {
				t.Fatalf("claim: ok=%v err=%v", ok, err)
			}
			if err := tc.release(name, provider.SyncResult{Etag: `"old"`}); err != nil {
				t.Fatalf("release: %v", err)
			}

			if err := testDB.DeleteRemote(ctx(), name); err != nil {
				t.Fatalf("delete remote: %v", err)
			}
			var rows int
			if err := testDB.Pool.QueryRow(ctx(), `SELECT count(*) FROM `+tc.table+` WHERE remote_name = $1`, name).Scan(&rows); err != nil || rows != 0 {
				t.Fatalf("sync state left after delete: rows=%d err=%v", rows, err)
			}

			create()
			ok, etag, err := tc.claim(name)
			if err != nil || !ok || etag != "" {
				t.Fatalf("recreated remote claim ok=%v etag=%q err=%v, want a fresh claim with no etag", ok, etag, err)
			}
		})
	}
}
