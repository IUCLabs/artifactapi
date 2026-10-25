package deb

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"golang.org/x/time/rate"

	"github.com/IUCLabs/artifactapi/internal/provider"
	"github.com/IUCLabs/artifactapi/internal/testsupport"
	"github.com/IUCLabs/artifactapi/pkg/models"
)

// fakeSyncStore is an in-memory SyncStore: the metadata cache (via the embedded
// fakeStore) plus the shared sync-state lease, whose claim mirrors the atomic
// semantics of the real SQL (recency gate AND no live lease).
type fakeSyncStore struct {
	*fakeStore

	mu         sync.Mutex
	remotes    []models.Remote
	leaseOwner map[string]string
	leaseExp   map[string]time.Time
	lastSynced map[string]time.Time
	etags      map[string]string
	retryAt    map[string]time.Time
	freshness  time.Duration
	interval   time.Duration
	phase      time.Duration
	attempts   int
}

func newFakeSyncStore() *fakeSyncStore {
	return &fakeSyncStore{
		fakeStore:  newFakeStore(),
		leaseOwner: map[string]string{},
		leaseExp:   map[string]time.Time{},
		lastSynced: map[string]time.Time{},
		etags:      map[string]string{},
		retryAt:    map[string]time.Time{},
	}
}

func (f *fakeSyncStore) ListGitHubDebRemotes(_ context.Context) ([]models.Remote, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]models.Remote(nil), f.remotes...), nil
}

func (f *fakeSyncStore) ClaimGitHubDebSyncLease(_ context.Context, name, owner string, freshness, lease, interval, phase time.Duration) (bool, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.freshness, f.interval, f.phase = freshness, interval, phase
	f.attempts++
	now := time.Now()
	ls, hasLS := f.lastSynced[name]
	exp, hasExp := f.leaseExp[name]
	freshOK := !hasLS || now.Sub(ls) >= freshness
	if ra, pending := f.retryAt[name]; pending {
		freshOK = !now.Before(ra)
	}
	leaseOK := !hasExp || exp.Before(now)
	if freshOK && leaseOK {
		f.leaseOwner[name] = owner
		f.leaseExp[name] = now.Add(lease)
		return true, f.etags[name], nil
	}
	return false, "", nil
}

func (f *fakeSyncStore) ReleaseGitHubDebSyncLease(_ context.Context, name, owner string, res provider.SyncResult) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.leaseOwner[name] != owner {
		return nil
	}
	if res.Failed {
		f.retryAt[name] = time.Now().Add(res.Backoff)
	} else {
		f.lastSynced[name] = time.Now()
		f.etags[name] = res.Etag
		delete(f.retryAt, name)
	}
	delete(f.leaseOwner, name)
	delete(f.leaseExp, name)
	return nil
}

func testSyncConfig() SyncConfig {
	return SyncConfig{RatePerSec: 1000, Burst: 100, Workers: 1, PollInterval: time.Hour}
}

// (a) A 304 conditional response must derive nothing: no asset fetches and
// changed=false, so an unchanged repo is nearly free.
func TestSyncerConditionalNotModifiedSkipsDerive(t *testing.T) {
	fx := newGitHubFixture(t, true)
	fx.etag = `"v1"`
	p := newTestProvider()
	store := newFakeStore()
	remote := fx.remote()

	etag1, changed, err := p.scanWithState(context.Background(), remote, store, "")
	if err != nil {
		t.Fatalf("first scan: %v", err)
	}
	if !changed || etag1 != `"v1"` {
		t.Fatalf("first scan changed=%v etag=%q, want true and \"v1\"", changed, etag1)
	}
	priorRange := fx.rangeHit["demo_1.2-3_amd64.deb"]
	if priorRange == 0 {
		t.Fatal("first scan should have fetched the asset control")
	}

	etag2, changed2, err := p.scanWithState(context.Background(), remote, store, etag1)
	if err != nil {
		t.Fatalf("second scan: %v", err)
	}
	if changed2 {
		t.Fatal("304 scan must report changed=false")
	}
	if etag2 != etag1 {
		t.Fatalf("etag changed across 304: %q -> %q", etag1, etag2)
	}
	if fx.notModHit != 1 {
		t.Fatalf("want exactly one 304 releases response, got %d", fx.notModHit)
	}
	if got := fx.rangeHit["demo_1.2-3_amd64.deb"]; got != priorRange {
		t.Fatalf("304 scan re-fetched asset control: %d -> %d", priorRange, got)
	}
}

// (b) On a real change, only the newly added asset is derived.
func TestSyncerIncrementalDerivesOnlyNewAsset(t *testing.T) {
	fx := newGitHubFixture(t, true)
	fx.etag = `"v1"`
	p := newTestProvider()
	store := newFakeStore()
	remote := fx.remote()

	if _, _, err := p.scanWithState(context.Background(), remote, store, ""); err != nil {
		t.Fatalf("first scan: %v", err)
	}
	demoRange := fx.rangeHit["demo_1.2-3_amd64.deb"]

	fx.debBytes["other_9_arm64.deb"] = testsupport.MinimalDeb("other", "9", "arm64")
	fx.etag = `"v2"`

	if _, changed, err := p.scanWithState(context.Background(), remote, store, `"v1"`); err != nil || !changed {
		t.Fatalf("second scan changed=%v err=%v", changed, err)
	}

	rows, _ := store.ListDebMetadataEntries(context.Background(), remote.Name)
	if len(rows) != 2 {
		t.Fatalf("want 2 cached rows after incremental derive, got %d", len(rows))
	}
	if got := fx.rangeHit["demo_1.2-3_amd64.deb"]; got != demoRange {
		t.Fatalf("already-cached asset was re-fetched: %d -> %d", demoRange, got)
	}
	if fx.rangeHit["other_9_arm64.deb"] == 0 {
		t.Fatal("newly added asset was not derived")
	}
}

// (c) The shared limiter caps the request rate.
func TestRateLimiterCapsRequestRate(t *testing.T) {
	fx := newGitHubFixture(t, true)
	p := newTestProvider()
	p.limiter = rate.NewLimiter(rate.Every(120*time.Millisecond), 1)
	remote := fx.remote()

	start := time.Now()
	for i := 0; i < 3; i++ {
		if _, _, _, err := p.fetchReleases(context.Background(), remote, ""); err != nil {
			t.Fatalf("fetchReleases %d: %v", i, err)
		}
	}
	if elapsed := time.Since(start); elapsed < 200*time.Millisecond {
		t.Fatalf("rate limiter did not throttle: 3 calls took %v, want >= 200ms", elapsed)
	}
}

// (d) Concurrent enqueues for the same remote coalesce to a single queued job.
func TestSyncerEnqueueDedup(t *testing.T) {
	store := newFakeSyncStore()
	p := newTestProvider()
	s := newSyncer(store, p, testSyncConfig())
	remote := models.Remote{Name: "acme-deb", PackageType: models.PackageGitHubDeb, MutableTTL: 3600}

	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); s.enqueue(remote, false) }()
	}
	wg.Wait()

	if got := len(s.jobs); got != 1 {
		t.Fatalf("want exactly 1 coalesced job, got %d", got)
	}
}

// (e) Prime-on-create enqueues a prime job.
func TestSyncerEnqueuePrime(t *testing.T) {
	store := newFakeSyncStore()
	p := newTestProvider()
	s := newSyncer(store, p, testSyncConfig())
	remote := models.Remote{Name: "acme-deb", PackageType: models.PackageGitHubDeb, MutableTTL: 3600}

	s.EnqueuePrime(remote)
	select {
	case job := <-s.jobs:
		if !job.prime || job.remote.Name != "acme-deb" {
			t.Fatalf("bad prime job: %+v", job)
		}
	default:
		t.Fatal("EnqueuePrime did not enqueue a job")
	}
}

// (f) A held lease prevents a second replica from scanning.
func TestSyncerLeasePreventsSecondReplica(t *testing.T) {
	fx := newGitHubFixture(t, true)
	fx.etag = `"v1"`
	store := newFakeSyncStore()
	p := newTestProvider()
	s := newSyncer(store, p, testSyncConfig())
	remote := fx.remote()

	claimed, _, err := store.ClaimGitHubDebSyncLease(context.Background(), remote.Name, "replica-1", time.Duration(remote.MutableTTL)*time.Second, syncLeaseDuration, 0, 0)
	if err != nil || !claimed {
		t.Fatalf("replica-1 claim: claimed=%v err=%v", claimed, err)
	}

	s.process(context.Background(), syncJob{remote: remote})

	if fx.releasesHit != 0 {
		t.Fatalf("second replica scanned while lease held: %d releases calls", fx.releasesHit)
	}
	if rows, _ := store.ListDebMetadataEntries(context.Background(), remote.Name); len(rows) != 0 {
		t.Fatalf("second replica derived metadata while lease held: %d rows", len(rows))
	}
}

// With the syncer wired and the cache empty, an index request enqueues a prime
// and returns a retryable 503 when it has not landed within the cold wait.
func TestServeRemoteColdStartReturns503(t *testing.T) {
	fx := newGitHubFixture(t, true)
	store := newFakeSyncStore()
	p := newTestProvider()
	p.coldWait = 300 * time.Millisecond
	_ = newSyncer(store, p, testSyncConfig()) // binds p.syncer, but no workers running
	remote := fx.remote()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/remote/acme-deb/Packages", nil)
	if !p.ServeRemote(rec, req, remote, "Packages", "https://x", store) {
		t.Fatal("ServeRemote did not handle Packages")
	}
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("cold empty cache must return 503, got %d", rec.Code)
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Fatal("503 should carry Retry-After")
	}
	if got := len(p.syncer.jobs); got != 1 {
		t.Fatalf("cold start did not enqueue a prime, jobs=%d", got)
	}
}

// With the cache warm, the same request serves the index immediately (no 503).
func TestServeRemoteWarmCacheServesImmediately(t *testing.T) {
	fx := newGitHubFixture(t, true)
	store := newFakeSyncStore()
	p := newTestProvider()
	_ = newSyncer(store, p, testSyncConfig())
	remote := fx.remote()

	if err := p.scan(context.Background(), remote, store); err != nil {
		t.Fatalf("warm scan: %v", err)
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/remote/acme-deb/Packages", nil)
	if !p.ServeRemote(rec, req, remote, "Packages", "https://x", store) {
		t.Fatal("ServeRemote did not handle Packages")
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("warm cache must serve 200, got %d body=%s", rec.Code, rec.Body.String())
	}
}

// A prime job (freshness 0) runs even right after a sync; a periodic job at the
// same moment is gated by the recency window.
func TestSyncerPrimeBypassesRecencyPeriodicDoesNot(t *testing.T) {
	fx := newGitHubFixture(t, true)
	fx.etag = `"v1"`
	store := newFakeSyncStore()
	p := newTestProvider()
	s := newSyncer(store, p, testSyncConfig())
	remote := fx.remote()

	var _ provider.RemoteMetadataStore = store

	s.process(context.Background(), syncJob{remote: remote, prime: true})
	if rows, _ := store.ListDebMetadataEntries(context.Background(), remote.Name); len(rows) != 1 {
		t.Fatalf("prime did not derive: %d rows", len(rows))
	}
	releasesAfterPrime := fx.releasesHit

	s.process(context.Background(), syncJob{remote: remote, prime: false})
	if fx.releasesHit != releasesAfterPrime {
		t.Fatalf("periodic scan ran inside recency window: %d -> %d releases calls", releasesAfterPrime, fx.releasesHit)
	}
}

// A transiently failed asset fails the scan: the ETag is not advanced, a retry
// is scheduled, and the retry against the unchanged release list derives it. An
// invalid package is skipped instead, since no retry can fix it.
func TestSyncerFailedAssetRetry(t *testing.T) {
	for _, tc := range []struct {
		name      string
		corrupt   bool
		wantRetry bool
	}{
		{name: "transient", wantRetry: true},
		{name: "invalid package", corrupt: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fx := newGitHubFixture(t, true)
			fx.etag = `"v1"`
			if tc.corrupt {
				fx.debBytes["other_9-9_arm64.deb"] = []byte("not a package")
			} else {
				fx.debBytes["other_9-9_arm64.deb"] = testsupport.MinimalDeb("other", "9-9", "arm64")
				fx.assetFail["other_9-9_arm64.deb"] = 1
			}
			store := newFakeSyncStore()
			s := newSyncer(store, newTestProvider(), testSyncConfig())
			remote := fx.remote()
			bg := context.Background()

			s.process(bg, syncJob{remote: remote, prime: true})
			_, pending := store.retryAt[remote.Name]
			if pending != tc.wantRetry {
				t.Fatalf("retry pending=%v, want %v", pending, tc.wantRetry)
			}
			if _, saved := store.etags[remote.Name]; saved == tc.wantRetry {
				t.Fatalf("etag saved=%v after first scan", saved)
			}
			if rows, _ := store.ListDebMetadataEntries(bg, remote.Name); len(rows) != 1 {
				t.Fatalf("healthy asset not served: %d rows", len(rows))
			}
			if !tc.wantRetry {
				return
			}

			store.mu.Lock()
			store.retryAt[remote.Name] = time.Now().Add(-time.Second)
			store.mu.Unlock()
			s.process(bg, syncJob{remote: remote})
			if _, pending := store.retryAt[remote.Name]; pending || store.etags[remote.Name] != `"v1"` {
				t.Fatalf("retry not recorded: pending=%v etag=%q", pending, store.etags[remote.Name])
			}
			if rows, _ := store.ListDebMetadataEntries(bg, remote.Name); len(rows) != 2 {
				t.Fatalf("retry did not derive the failed asset: %d rows", len(rows))
			}
		})
	}
}

// A periodic check claims with sync_interval as its freshness and passes the
// remote's slot schedule; unset, it falls back to mutable_ttl with no schedule.
func TestSyncerHonoursSyncInterval(t *testing.T) {
	for _, interval := range []int{0, 7200} {
		store := newFakeSyncStore()
		s := newSyncer(store, newTestProvider(), testSyncConfig())
		store.lastSynced["sched"] = time.Now()
		remote := models.Remote{Name: "sched", MutableTTL: 90, SyncInterval: interval}
		wantInterval, wantPhase := remote.SyncSchedule()
		s.process(context.Background(), syncJob{remote: remote})
		if want := remote.SyncPeriod(0); store.freshness != want || store.interval != wantInterval || store.phase != wantPhase {
			t.Errorf("sync_interval=%d: claim freshness/interval/phase = %v/%v/%v, want %v/%v/%v",
				interval, store.freshness, store.interval, store.phase, want, wantInterval, wantPhase)
		}
	}
}

// Create-time primes are skipped for a scheduled remote so remotes created
// together spread out; a cold-cache prime still claims with no schedule.
func TestSyncerScheduledPrime(t *testing.T) {
	store := newFakeSyncStore()
	s := newSyncer(store, newTestProvider(), testSyncConfig())
	remote := models.Remote{Name: "fresh", SyncInterval: 3600}

	s.EnqueuePrime(remote)
	if len(s.jobs) != 0 {
		t.Fatal("create-time prime enqueued for a sync_interval remote")
	}
	s.process(context.Background(), syncJob{remote: remote, prime: true})
	if store.attempts != 1 || store.freshness != 0 || store.interval != 0 {
		t.Fatalf("cold-cache prime: attempts=%d freshness=%v interval=%v, want 1/0/0", store.attempts, store.freshness, store.interval)
	}
}
