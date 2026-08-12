package alpine

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
}

func newFakeSyncStore() *fakeSyncStore {
	return &fakeSyncStore{
		fakeStore:  newFakeStore(),
		leaseOwner: map[string]string{},
		leaseExp:   map[string]time.Time{},
		lastSynced: map[string]time.Time{},
		etags:      map[string]string{},
	}
}

func (f *fakeSyncStore) ListGitHubAlpineRemotes(_ context.Context) ([]models.Remote, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]models.Remote(nil), f.remotes...), nil
}

func (f *fakeSyncStore) ClaimGitHubAlpineSyncLease(_ context.Context, name, owner string, freshness, lease time.Duration) (bool, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	now := time.Now()
	ls, hasLS := f.lastSynced[name]
	exp, hasExp := f.leaseExp[name]
	freshOK := !hasLS || now.Sub(ls) >= freshness
	leaseOK := !hasExp || exp.Before(now)
	if freshOK && leaseOK {
		f.leaseOwner[name] = owner
		f.leaseExp[name] = now.Add(lease)
		return true, f.etags[name], nil
	}
	return false, "", nil
}

func (f *fakeSyncStore) ReleaseGitHubAlpineSyncLease(_ context.Context, name, owner, etag string, syncedAt time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.leaseOwner[name] != owner {
		return nil
	}
	f.lastSynced[name] = syncedAt
	f.etags[name] = etag
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
	priorRange := fx.rangeHit["demo-1.2.3-r0.apk"]
	if priorRange == 0 {
		t.Fatal("first scan should have fetched the asset .PKGINFO")
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
	if got := fx.rangeHit["demo-1.2.3-r0.apk"]; got != priorRange {
		t.Fatalf("304 scan re-fetched asset .PKGINFO: %d -> %d", priorRange, got)
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
	demoRange := fx.rangeHit["demo-1.2.3-r0.apk"]

	fx.apkBytes["other-9-r0.apk"] = testsupport.MinimalApk("other", "9-r0", "aarch64")
	fx.etag = `"v2"`

	if _, changed, err := p.scanWithState(context.Background(), remote, store, `"v1"`); err != nil || !changed {
		t.Fatalf("second scan changed=%v err=%v", changed, err)
	}

	rows, _ := store.ListAlpineMetadataEntries(context.Background(), remote.Name)
	if len(rows) != 2 {
		t.Fatalf("want 2 cached rows after incremental derive, got %d", len(rows))
	}
	if got := fx.rangeHit["demo-1.2.3-r0.apk"]; got != demoRange {
		t.Fatalf("already-cached asset was re-fetched: %d -> %d", demoRange, got)
	}
	if fx.rangeHit["other-9-r0.apk"] == 0 {
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
	remote := models.Remote{Name: "acme-apk", PackageType: models.PackageGitHubAlpine, MutableTTL: 3600}

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
	remote := models.Remote{Name: "acme-apk", PackageType: models.PackageGitHubAlpine, MutableTTL: 3600}

	s.EnqueuePrime(remote)
	select {
	case job := <-s.jobs:
		if !job.prime || job.remote.Name != "acme-apk" {
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

	claimed, _, err := store.ClaimGitHubAlpineSyncLease(context.Background(), remote.Name, "replica-1", time.Duration(remote.MutableTTL)*time.Second, syncLeaseDuration)
	if err != nil || !claimed {
		t.Fatalf("replica-1 claim: claimed=%v err=%v", claimed, err)
	}

	s.process(context.Background(), syncJob{remote: remote})

	if fx.releasesHit != 0 {
		t.Fatalf("second replica scanned while lease held: %d releases calls", fx.releasesHit)
	}
	if rows, _ := store.ListAlpineMetadataEntries(context.Background(), remote.Name); len(rows) != 0 {
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
	req := httptest.NewRequest(http.MethodGet, "/api/v1/remote/acme-apk/x86_64/APKINDEX.tar.gz", nil)
	if !p.ServeRemote(rec, req, remote, "x86_64/APKINDEX.tar.gz", "https://x", store) {
		t.Fatal("ServeRemote did not handle APKINDEX")
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
	req := httptest.NewRequest(http.MethodGet, "/api/v1/remote/acme-apk/x86_64/APKINDEX.tar.gz", nil)
	if !p.ServeRemote(rec, req, remote, "x86_64/APKINDEX.tar.gz", "https://x", store) {
		t.Fatal("ServeRemote did not handle APKINDEX")
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
	if rows, _ := store.ListAlpineMetadataEntries(context.Background(), remote.Name); len(rows) != 1 {
		t.Fatalf("prime did not derive: %d rows", len(rows))
	}
	releasesAfterPrime := fx.releasesHit

	s.process(context.Background(), syncJob{remote: remote, prime: false})
	if fx.releasesHit != releasesAfterPrime {
		t.Fatalf("periodic scan ran inside recency window: %d -> %d releases calls", releasesAfterPrime, fx.releasesHit)
	}
}
