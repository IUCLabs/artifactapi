package rpm

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/IUCLabs/artifactapi/internal/provider"
	"github.com/IUCLabs/artifactapi/pkg/models"
)

// fpStore adds a fingerprint and a full-read counter to fakeStore.
type fpStore struct {
	*fakeStore
	lists, fps     int
	seq            int64
	fpErr, listErr error
	gate           chan struct{} // when set, full reads block until it is closed
}

func (s *fpStore) ListRPMMetadataEntries(ctx context.Context, name string) ([]provider.RPMMetadata, error) {
	s.mu.Lock()
	s.lists++
	gate, err := s.gate, s.listErr
	s.mu.Unlock()
	if gate != nil {
		<-gate
	}
	if err != nil {
		return nil, err
	}
	return s.fakeStore.ListRPMMetadataEntries(ctx, name)
}

func (s *fpStore) InsertRPMMetadata(ctx context.Context, m *provider.RPMMetadata) error {
	s.mu.Lock()
	_, exists := s.rows[m.FilePath]
	s.mu.Unlock()
	if err := s.fakeStore.InsertRPMMetadata(ctx, m); err != nil || exists {
		return err // ON CONFLICT DO NOTHING leaves max(id) unchanged
	}
	s.mu.Lock()
	s.seq++
	s.mu.Unlock()
	return nil
}

func (s *fpStore) RPMMetadataFingerprint(_ context.Context, _ string) (int64, int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fps++
	return int64(len(s.rows)), s.seq, s.fpErr
}

func (s *fpStore) fpCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.fps
}

func (s *fpStore) listCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lists
}

func newCacheFixture(t *testing.T, cache bool) (*GitHubProvider, *fpStore, models.Remote) {
	t.Helper()
	remote := models.Remote{Name: "cached-rpm", PackageType: models.PackageGitHubRPM, MutableTTL: 3600, CacheRepodata: cache}
	p := newGitHubProvider()
	p.lastScan[remote.Name] = time.Now()
	store := &fpStore{fakeStore: newFakeStore()}
	addPkg(t, store, "alpha")
	return p, store, remote
}

func addPkg(t *testing.T, store *fpStore, name string) {
	t.Helper()
	if err := store.InsertRPMMetadata(context.Background(), &provider.RPMMetadata{
		RepoName: "cached-rpm", FilePath: name + "-1-1.noarch.rpm", ContentHash: "sha256:" + name,
		Name: name, Version: "1", Release: "1", Arch: "noarch", Group: "Unspecified",
		Files: []provider.RPMFile{{Path: "/usr/bin/" + name}},
	}); err != nil {
		t.Fatal(err)
	}
}

func serveReq(t *testing.T, p *GitHubProvider, remote models.Remote, store *fpStore, req *http.Request, path string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	if !p.ServeRemote(rec, req, remote, path, "https://proxy.example", store) {
		t.Errorf("%s not handled", path)
	}
	return rec
}

func serve(t *testing.T, p *GitHubProvider, remote models.Remote, store *fpStore, path string) []byte {
	t.Helper()
	rec := serveReq(t, p, remote, store, httptest.NewRequest(http.MethodGet, "/x", nil), path)
	if rec.Code != http.StatusOK {
		t.Fatalf("%s: code %d: %s", path, rec.Code, rec.Body.String())
	}
	return rec.Body.Bytes()
}

var repodataPaths = []string{
	"repodata/repomd.xml",
	"repodata/x-primary.xml.gz",
	"repodata/x-filelists.xml.gz",
	"repodata/x-other.xml.gz",
}

func TestCachedRepodataHitSkipsRegeneration(t *testing.T) {
	p, store, remote := newCacheFixture(t, true)

	repomd := serve(t, p, remote, store, "repodata/repomd.xml")
	primary := serve(t, p, remote, store, "repodata/x-primary.xml.gz")
	if n := store.listCount(); n != 1 {
		t.Fatalf("full metadata reads = %d, want 1", n)
	}
	if !strings.Contains(string(repomd), sha256Hex(primary)+"-primary.xml.gz") {
		t.Fatalf("cached repomd does not reference cached primary:\n%s", repomd)
	}
}

func TestCachedRepodataRebuildsOnDataChange(t *testing.T) {
	p, store, remote := newCacheFixture(t, true)
	before := serve(t, p, remote, store, "repodata/repomd.xml")

	addPkg(t, store, "beta")
	after := serve(t, p, remote, store, "repodata/repomd.xml")
	primary := serve(t, p, remote, store, "repodata/x-primary.xml.gz")

	if string(before) == string(after) {
		t.Fatal("repomd unchanged after metadata change")
	}
	if !strings.Contains(string(after), sha256Hex(primary)+"-primary.xml.gz") {
		t.Fatal("rebuilt repomd does not reference rebuilt primary")
	}
	if !strings.Contains(gunzip(t, primary), "<name>beta</name>") {
		t.Fatal("rebuilt primary missing new package")
	}
	if n := store.listCount(); n != 2 {
		t.Fatalf("full metadata reads = %d, want 2", n)
	}
}

func TestUncachedRepodataMatchesCachedAndReadsEveryTime(t *testing.T) {
	p, store, remote := newCacheFixture(t, true)
	cached := map[string]string{}
	for _, path := range repodataPaths {
		cached[path] = string(serve(t, p, remote, store, path))
	}

	remote.CacheRepodata = false
	reads := store.listCount()
	for _, path := range repodataPaths {
		if got := string(serve(t, p, remote, store, path)); got != cached[path] {
			t.Fatalf("%s differs between cached and uncached", path)
		}
	}
	if n := store.listCount() - reads; n != len(repodataPaths) {
		t.Fatalf("uncached full reads = %d, want %d", n, len(repodataPaths))
	}
	if _, ok := p.repodataCache[remote.Name]; ok {
		t.Fatal("disabling cache_repodata should drop the cached index")
	}
}

func TestCachedRepodataRebuildsOnSameCountReinsert(t *testing.T) {
	p, store, remote := newCacheFixture(t, true)
	addPkg(t, store, "alpha") // conflicting insert: fingerprint must not move
	serve(t, p, remote, store, "repodata/repomd.xml")
	serve(t, p, remote, store, "repodata/repomd.xml")
	if n := store.listCount(); n != 1 {
		t.Fatalf("full reads after conflicting insert = %d, want 1", n)
	}

	if err := store.DeleteRPMMetadata(context.Background(), remote.Name, "alpha-1-1.noarch.rpm"); err != nil {
		t.Fatal(err)
	}
	addPkg(t, store, "alpha")
	serve(t, p, remote, store, "repodata/repomd.xml")
	if n := store.listCount(); n != 2 {
		t.Fatalf("full reads after same-count reinsert = %d, want 2", n)
	}
}

func TestCachedRepodataEvictsIdleRemotes(t *testing.T) {
	p, store, remote := newCacheFixture(t, true)
	serve(t, p, remote, store, "repodata/repomd.xml")
	p.repodataCache["gone"] = cachedRepodata{used: time.Now().Add(-2 * repodataIdleTTL), repodata: &repodata{}}
	p.repodataSwept = time.Time{}

	serve(t, p, remote, store, "repodata/repomd.xml")
	if _, ok := p.repodataCache["gone"]; ok {
		t.Fatal("idle remote's index not evicted")
	}
	if _, ok := p.repodataCache[remote.Name]; !ok {
		t.Fatal("active remote's index evicted")
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not reached")
		}
		time.Sleep(time.Millisecond)
	}
}

// sfWaiters counts goroutines parked on an in-flight singleflight call.
func sfWaiters() int {
	buf := make([]byte, 1<<20)
	for {
		n := runtime.Stack(buf, true)
		if n < len(buf) {
			buf = buf[:n]
			break
		}
		buf = make([]byte, 2*len(buf))
	}
	waiters := 0
	for _, g := range strings.Split(string(buf), "\n\n") {
		if strings.Contains(g, "sync.(*WaitGroup).Wait") && strings.Contains(g, "singleflight.(*Group).Do(") {
			waiters++
		}
	}
	return waiters
}

func TestCachedRepodataCoalescesConcurrentBuilds(t *testing.T) {
	p, store, remote := newCacheFixture(t, true)
	gate := make(chan struct{})
	store.gate = gate
	const n = 8
	var wg sync.WaitGroup
	codes := make([]int, n)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			codes[i] = serveReq(t, p, remote, store, httptest.NewRequest(http.MethodGet, "/x", nil), repodataPaths[i%len(repodataPaths)]).Code
		}()
	}
	waitFor(t, func() bool { return store.listCount() == 1 && sfWaiters() == n-1 })
	close(gate)
	wg.Wait()
	for i, c := range codes {
		if c != http.StatusOK {
			t.Errorf("request %d: code %d", i, c)
		}
	}
	if got := store.listCount(); got != 1 {
		t.Fatalf("full metadata reads = %d, want 1", got)
	}
}

func TestCachedRepodataLeaderCancelDoesNotFailWaiters(t *testing.T) {
	p, store, remote := newCacheFixture(t, true)
	gate := make(chan struct{})
	store.gate = gate
	leaderCtx, cancelLeader := context.WithCancel(context.Background())
	results := make(chan int, 2)
	go func() {
		req := httptest.NewRequest(http.MethodGet, "/x", nil).WithContext(leaderCtx)
		results <- serveReq(t, p, remote, store, req, "repodata/repomd.xml").Code
	}()
	waitFor(t, func() bool { return store.listCount() == 1 })
	go func() {
		results <- serveReq(t, p, remote, store, httptest.NewRequest(http.MethodGet, "/x", nil), "repodata/repomd.xml").Code
	}()
	waitFor(t, func() bool { return sfWaiters() == 1 })
	cancelLeader()
	close(gate)
	for range 2 {
		if c := <-results; c != http.StatusOK {
			t.Fatalf("code %d after leader cancel, want 200", c)
		}
	}
	if got := store.listCount(); got != 1 {
		t.Fatalf("full metadata reads = %d, want 1", got)
	}
}

func TestCachedRepodataOlderBuildDoesNotOverwriteNewer(t *testing.T) {
	p, store, remote := newCacheFixture(t, true)
	newer := &repodata{}
	p.repodataCache[remote.Name] = cachedRepodata{count: 99, maxID: 99, seq: 1 << 40, used: time.Now(), repodata: newer}
	serve(t, p, remote, store, "repodata/repomd.xml")
	if p.repodataCache[remote.Name].repodata != newer {
		t.Fatal("older build replaced a newer cache entry")
	}
}

func TestCachedRepodataErrors(t *testing.T) {
	secret := errors.New("pq: password authentication failed for user secret")
	for _, tc := range []struct {
		name           string
		fpErr, listErr error
		code           int
	}{
		{"fingerprint", secret, nil, http.StatusInternalServerError},
		{"list", nil, secret, http.StatusInternalServerError},
		{"fingerprint deadline", context.DeadlineExceeded, nil, http.StatusServiceUnavailable},
		{"list canceled", nil, context.Canceled, http.StatusServiceUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, store, remote := newCacheFixture(t, true)
			store.fpErr, store.listErr = tc.fpErr, tc.listErr
			rec := serveReq(t, p, remote, store, httptest.NewRequest(http.MethodGet, "/x", nil), "repodata/repomd.xml")
			if rec.Code != tc.code {
				t.Fatalf("code %d, want %d", rec.Code, tc.code)
			}
			if strings.Contains(rec.Body.String(), "secret") {
				t.Fatalf("DB error leaked: %q", rec.Body.String())
			}
			if retry := rec.Header().Get("Retry-After"); (tc.code == http.StatusServiceUnavailable) != (retry != "") {
				t.Fatalf("Retry-After = %q for %d", retry, tc.code)
			}
			if _, ok := p.repodataCache[remote.Name]; ok {
				t.Fatal("failed build was cached")
			}
		})
	}
}

func TestCachedRepodataUnknownTail404sWithoutDB(t *testing.T) {
	p, store, remote := newCacheFixture(t, true)
	rec := serveReq(t, p, remote, store, httptest.NewRequest(http.MethodGet, "/x", nil), "repodata/bogus.txt")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("code %d, want 404", rec.Code)
	}
	if store.fpCount() != 0 || store.listCount() != 0 {
		t.Fatalf("unknown tail hit the DB: fps=%d lists=%d", store.fpCount(), store.listCount())
	}
}

func TestCacheEmptyUsesFingerprint(t *testing.T) {
	p := newGitHubProvider()
	store := &fpStore{fakeStore: newFakeStore()}
	if !p.cacheEmpty(context.Background(), store, "cached-rpm") {
		t.Fatal("empty store not reported empty")
	}
	addPkg(t, store, "alpha")
	if p.cacheEmpty(context.Background(), store, "cached-rpm") {
		t.Fatal("populated store reported empty")
	}
	store.fpErr = errors.New("db down")
	if p.cacheEmpty(context.Background(), store, "cached-rpm") {
		t.Fatal("fingerprint error must not report empty")
	}
	if store.listCount() != 0 {
		t.Fatalf("cacheEmpty did full reads: %d", store.listCount())
	}
}
