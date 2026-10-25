package database

import (
	"context"
	"fmt"
	"math"
	"sync/atomic"
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
	claimed, etag, err := testDB.ClaimGitHubSyncLease(ctx(), name, "replica-1", freshness, lease, 0, 0)
	if err != nil || !claimed {
		t.Fatalf("replica-1 first claim: claimed=%v err=%v", claimed, err)
	}
	if etag != "" {
		t.Fatalf("initial etag should be empty, got %q", etag)
	}

	// A second replica cannot claim while the lease is held.
	claimed2, _, err := testDB.ClaimGitHubSyncLease(ctx(), name, "replica-2", freshness, lease, 0, 0)
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
	claimed3, _, err := testDB.ClaimGitHubSyncLease(ctx(), name, "replica-2", freshness, lease, 0, 0)
	if err != nil {
		t.Fatalf("replica-2 recency claim err: %v", err)
	}
	if claimed3 {
		t.Fatal("periodic claim succeeded inside the freshness window")
	}

	// A prime (freshness 0) bypasses recency and reads the persisted etag.
	claimed4, etag4, err := testDB.ClaimGitHubSyncLease(ctx(), name, "replica-2", 0, lease, 0, 0)
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
		ok, etag, err := testDB.ClaimGitHubSyncLease(ctx(), name, "r1", f, lease, 0, 0)
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
				return testDB.ClaimGitHubSyncLease(ctx(), n, "r1", time.Hour, time.Minute, 0, 0)
			},
			func(n string, res provider.SyncResult) error {
				return testDB.ReleaseGitHubSyncLease(ctx(), n, "r1", res)
			}},
		{"github_deb_sync_state", models.PackageGitHubDeb,
			func(n string) (bool, string, error) {
				return testDB.ClaimGitHubDebSyncLease(ctx(), n, "r1", time.Hour, time.Minute, 0, 0)
			},
			func(n string, res provider.SyncResult) error {
				return testDB.ReleaseGitHubDebSyncLease(ctx(), n, "r1", res)
			}},
		{"github_alpine_sync_state", models.PackageGitHubAlpine,
			func(n string) (bool, string, error) {
				return testDB.ClaimGitHubAlpineSyncLease(ctx(), n, "r1", time.Hour, time.Minute, 0, 0)
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

type syncTable struct {
	table string
	pt    models.PackageType
	claim func(name, owner string, freshness, interval, phase time.Duration) (bool, error)
}

func syncTables() []syncTable {
	wrap := func(fn func(context.Context, string, string, time.Duration, time.Duration, time.Duration, time.Duration) (bool, string, error)) func(string, string, time.Duration, time.Duration, time.Duration) (bool, error) {
		return func(name, owner string, freshness, interval, phase time.Duration) (bool, error) {
			ok, _, err := fn(ctx(), name, owner, freshness, 15*time.Minute, interval, phase)
			return ok, err
		}
	}
	return []syncTable{
		{"github_rpm_sync_state", models.PackageGitHubRPM, wrap(testDB.ClaimGitHubSyncLease)},
		{"github_deb_sync_state", models.PackageGitHubDeb, wrap(testDB.ClaimGitHubDebSyncLease)},
		{"github_alpine_sync_state", models.PackageGitHubAlpine, wrap(testDB.ClaimGitHubAlpineSyncLease)},
	}
}

var syncSeq atomic.Int64

// seedSynced creates a remote of tc's type created createdAgo before the DB
// clock and, when syncedAgo >= 0, a released lease last synced syncedAgo before it.
func seedSynced(t *testing.T, tc syncTable, createdAgo, syncedAgo time.Duration) string {
	t.Helper()
	name := fmt.Sprintf("sync-%s-%d", tc.table, syncSeq.Add(1))
	if err := testDB.CreateRemote(ctx(), &models.Remote{
		Name: name, PackageType: tc.pt, RepoType: models.RepoTypeRemote,
		BaseURL: "https://api.github.com/repos/acme/tools", ReleasesRemote: "github",
	}); err != nil {
		t.Fatalf("create remote: %v", err)
	}
	if _, err := testDB.Pool.Exec(ctx(), `UPDATE remotes SET created_at = now() - make_interval(secs => $2) WHERE name = $1`, name, createdAgo.Seconds()); err != nil {
		t.Fatalf("seed created_at: %v", err)
	}
	if syncedAgo >= 0 {
		if _, err := testDB.Pool.Exec(ctx(), `INSERT INTO `+tc.table+` (remote_name, last_synced_at) VALUES ($1, now() - make_interval(secs => $2))`, name, syncedAgo.Seconds()); err != nil {
			t.Fatalf("seed sync state: %v", err)
		}
	}
	return name
}

func mustClaim(t *testing.T, tc syncTable, name, owner string, freshness, interval, phase time.Duration) bool {
	t.Helper()
	ok, err := tc.claim(name, owner, freshness, interval, phase)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	return ok
}

const neverSynced = -1

// With no interval the claim keeps the freshness-since-last-sync predicate and
// a new remote is due at once.
func TestSyncLeaseUnsetIntervalFreshness(t *testing.T) {
	requireDB(t)
	const ttl = time.Hour
	for _, tc := range syncTables() {
		t.Run(tc.table, func(t *testing.T) {
			for _, c := range []struct {
				ago       time.Duration
				freshness time.Duration
				due       bool
			}{
				{neverSynced, ttl, true},
				{ttl + time.Minute, ttl, true},
				{ttl - time.Minute, ttl, false},
				{time.Second, ttl, false},
				{time.Second, 0, true},
			} {
				name := seedSynced(t, tc, time.Second, c.ago)
				if got := mustClaim(t, tc, name, "r1", c.freshness, 0, 0); got != c.due {
					t.Errorf("synced %v ago, freshness %v: claimed=%v, want %v", c.ago, c.freshness, got, c.due)
				}
			}
		})
	}
}

// With an interval the slot comes from the DB clock: a remote is due only when
// created and last synced before the current slot, regardless of freshness,
// and still one replica at a time.
func TestSyncLeaseSlot(t *testing.T) {
	requireDB(t)
	var epoch float64
	if err := testDB.Pool.QueryRow(ctx(), `SELECT extract(epoch FROM now())::float8`).Scan(&epoch); err != nil {
		t.Fatalf("db now: %v", err)
	}
	const interval = time.Hour
	// Pick the phase that puts the current slot's start 30m before the DB now.
	phase := time.Duration(math.Mod(epoch-1800, interval.Seconds()) * float64(time.Second))
	const day = 24 * time.Hour
	for _, tc := range syncTables() {
		t.Run(tc.table, func(t *testing.T) {
			for _, c := range []struct {
				desc            string
				createdAgo, ago time.Duration
				freshness       time.Duration
				due             bool
			}{
				{"created inside the slot", 10 * time.Second, neverSynced, 0, false},
				{"created before the slot", 35 * time.Minute, neverSynced, 0, true},
				{"synced inside the slot", day, 25 * time.Minute, 0, false},
				{"synced before the slot", day, 35 * time.Minute, 10 * 365 * day, true},
			} {
				name := seedSynced(t, tc, c.createdAgo, c.ago)
				if got := mustClaim(t, tc, name, "r1", c.freshness, interval, phase); got != c.due {
					t.Errorf("%s: claimed=%v, want %v", c.desc, got, c.due)
				}
				if c.due && mustClaim(t, tc, name, "r2", 0, interval, phase) {
					t.Errorf("%s: second replica claimed a held lease", c.desc)
				}
			}
		})
	}
}

// slotPhase returns the phase that puts the current interval slot's start
// startAgo before the DB clock.
func slotPhase(t *testing.T, interval, startAgo time.Duration) time.Duration {
	t.Helper()
	var epoch float64
	if err := testDB.Pool.QueryRow(ctx(), `SELECT extract(epoch FROM now())::float8`).Scan(&epoch); err != nil {
		t.Fatalf("db now: %v", err)
	}
	return time.Duration(math.Mod(epoch-startAgo.Seconds(), interval.Seconds()) * float64(time.Second))
}

func setSyncedAgo(t *testing.T, tc syncTable, name string, ago time.Duration) {
	t.Helper()
	if _, err := testDB.Pool.Exec(ctx(), `UPDATE `+tc.table+` SET last_synced_at = now() - make_interval(secs => $2), sync_lease_owner = '', sync_lease_expires = NULL WHERE remote_name = $1`, name, ago.Seconds()); err != nil {
		t.Fatalf("set last_synced_at: %v", err)
	}
}

// Toggling sync_interval on a remote with sync state neither re-syncs at once
// nor leaves it waiting longer than one interval (or freshness) since its last
// sync. Elapsed time is simulated by moving last_synced_at and the slot start
// back together, relative to the DB clock.
func TestSyncLeaseIntervalChange(t *testing.T) {
	requireDB(t)
	const (
		interval  = time.Hour
		freshness = time.Hour
		day       = 24 * time.Hour
	)
	for _, tc := range syncTables() {
		t.Run(tc.table, func(t *testing.T) {
			t.Run("0 to N", func(t *testing.T) {
				name := seedSynced(t, tc, day, 25*time.Minute)
				if mustClaim(t, tc, name, "r1", freshness, 0, 0) {
					t.Fatal("claimed inside freshness before the change")
				}
				if mustClaim(t, tc, name, "r1", freshness, interval, slotPhase(t, interval, 30*time.Minute)) {
					t.Fatal("synced inside the current slot, yet claimed right after the change")
				}
				// 30m later the next slot opens; the last sync is 55m old.
				setSyncedAgo(t, tc, name, 55*time.Minute)
				if !mustClaim(t, tc, name, "r1", freshness, interval, slotPhase(t, interval, time.Second)) {
					t.Fatal("not due at the first slot after the last sync")
				}
			})
			t.Run("N to 0", func(t *testing.T) {
				name := seedSynced(t, tc, day, 10*time.Minute)
				if mustClaim(t, tc, name, "r1", freshness, interval, slotPhase(t, interval, 30*time.Minute)) {
					t.Fatal("claimed inside the slot before the change")
				}
				if mustClaim(t, tc, name, "r1", freshness, 0, 0) {
					t.Fatal("synced inside freshness, yet claimed right after the change")
				}
				setSyncedAgo(t, tc, name, freshness+time.Second)
				if !mustClaim(t, tc, name, "r1", freshness, 0, 0) {
					t.Fatal("not due once freshness elapsed after the change")
				}
			})
		})
	}
}

// The created_at gate only holds back the first insert of a remote created
// inside the current slot; once a state row exists the row's own sync and
// retry state decide.
func TestSyncLeaseCreatedGateInsertOnly(t *testing.T) {
	requireDB(t)
	const interval = time.Hour
	phase := slotPhase(t, interval, 30*time.Minute)
	for _, tc := range syncTables() {
		t.Run(tc.table, func(t *testing.T) {
			fresh := seedSynced(t, tc, 10*time.Second, neverSynced)
			if mustClaim(t, tc, fresh, "r1", 0, interval, phase) {
				t.Fatal("remote created inside the slot claimed with no state row")
			}

			name := seedSynced(t, tc, 10*time.Second, neverSynced)
			if _, err := testDB.Pool.Exec(ctx(), `INSERT INTO `+tc.table+` (remote_name, sync_failures, next_retry_at) VALUES ($1, 1, now() - interval '1 second')`, name); err != nil {
				t.Fatalf("seed failed prime: %v", err)
			}
			if !mustClaim(t, tc, name, "r1", 0, interval, phase) {
				t.Fatal("created_at gate blocked a remote with a state row whose retry is due")
			}
		})
	}
}
