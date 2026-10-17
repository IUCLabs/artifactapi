package proxy

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	_ "github.com/IUCLabs/artifactapi/internal/provider/rpm"
	"github.com/IUCLabs/artifactapi/internal/storage"
	"github.com/IUCLabs/artifactapi/pkg/models"
)

// changingUpstream serves every path with the current revision, as a mirror
// does after a sync replaces its repodata. It answers 304 to a matching
// If-None-Match and counts conditional requests.
func changingUpstream(t *testing.T) (*httptest.Server, *atomic.Value, *atomic.Int32) {
	t.Helper()
	var rev atomic.Value
	var conditional atomic.Int32
	rev.Store("rev1")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		v := rev.Load().(string)
		etag := `"` + v + `"`
		w.Header().Set("ETag", etag)
		if inm := r.Header.Get("If-None-Match"); inm != "" {
			conditional.Add(1)
			if inm == etag {
				w.WriteHeader(http.StatusNotModified)
				return
			}
		}
		_, _ = w.Write([]byte(v + ":" + r.URL.Path))
	}))
	t.Cleanup(srv.Close)
	return srv, &rev, &conditional
}

func fetchBody(t *testing.T, r models.Remote, path string) string {
	t.Helper()
	res, err := testEngine.Fetch(context.Background(), r, path, prov(t, models.PackageRPM))
	if err != nil {
		t.Fatalf("fetch %s: %v", path, err)
	}
	return readAll(t, res)
}

func rpmRemote(t *testing.T, name, baseURL string) models.Remote {
	return seed(t, models.Remote{Name: name, PackageType: models.PackageRPM, RepoType: models.RepoTypeRemote, BaseURL: baseURL, MutableTTL: 7200, CheckMutable: true})
}

// cached reports which cache layers hold path: artifact row, index object,
// Redis TTL key, Redis ETag key.
type cached struct{ row, index, ttl, etag bool }

// Mutable indexes live in the S3 index; immutable blobs get an artifact row.
var (
	indexCached = cached{index: true, ttl: true, etag: true}
	blobCached  = cached{row: true, ttl: true}
)

func layers(t *testing.T, remote, path string) cached {
	t.Helper()
	ctx := context.Background()
	var c cached
	_, err := testDB.GetArtifact(ctx, remote, path)
	c.row = err == nil
	c.index, err = testEngine.store.Exists(ctx, storage.IndexKey(remote, path))
	if err != nil {
		t.Fatalf("stat index %s: %v", path, err)
	}
	c.ttl, _ = testCache.CheckTTL(ctx, remote, path)
	etag, _ := testCache.GetETag(ctx, remote, path)
	c.etag = etag != ""
	return c
}

func TestEvictMutableIndexRefetches(t *testing.T) {
	requireStack(t)
	srv, rev, conditional := changingUpstream(t)
	r := rpmRemote(t, "evict-idx", srv.URL)
	const path = "8/Everything/x86_64/repodata/repomd.xml"

	if got := fetchBody(t, r, path); got != "rev1:/"+path {
		t.Fatalf("initial fetch = %q", got)
	}
	if c := layers(t, r.Name, path); c != indexCached {
		t.Fatalf("before evict = %+v, want %+v", c, indexCached)
	}
	rev.Store("rev2")
	if got := fetchBody(t, r, path); got != "rev1:/"+path {
		t.Fatalf("within TTL = %q, want cached rev1", got)
	}
	if err := testEngine.Evict(context.Background(), r.Name, path); err != nil {
		t.Fatalf("evict: %v", err)
	}
	if c := layers(t, r.Name, path); c != (cached{}) {
		t.Fatalf("after evict = %+v, want every layer gone", c)
	}
	conditional.Store(0)
	if got := fetchBody(t, r, path); got != "rev2:/"+path {
		t.Fatalf("after evict = %q, want rev2", got)
	}
	if n := conditional.Load(); n != 0 {
		t.Errorf("post-evict fetch revalidated with the evicted ETag (%d conditional requests)", n)
	}
}

func TestEvictImmutableBlobDropsRow(t *testing.T) {
	requireStack(t)
	srv, _, _ := changingUpstream(t)
	r := rpmRemote(t, "evict-blob", srv.URL)
	const path = "8/Everything/x86_64/Packages/a/a-1.0-1.el8.x86_64.rpm"
	fetchBody(t, r, path)
	if c := layers(t, r.Name, path); c != blobCached {
		t.Fatalf("before evict = %+v, want %+v", c, blobCached)
	}
	if err := testEngine.Evict(context.Background(), r.Name, path); err != nil {
		t.Fatalf("evict: %v", err)
	}
	if c := layers(t, r.Name, path); c != (cached{}) {
		t.Fatalf("after evict = %+v, want every layer gone", c)
	}
}

func TestEvictRejectsNonDirectoryWildcard(t *testing.T) {
	requireStack(t)
	srv, _, _ := changingUpstream(t)
	r := rpmRemote(t, "evict-bare", srv.URL)
	const path = "8/Everything/x86_64/repodata/repomd.xml"
	fetchBody(t, r, path)

	for _, bad := range []string{"*", "8*", "8/Every*"} {
		var pe *ProxyError
		if err := testEngine.Evict(context.Background(), r.Name, bad); !errors.As(err, &pe) || pe.Status != http.StatusBadRequest {
			t.Errorf("evict %s = %v, want 400", bad, err)
		}
	}
	if c := layers(t, r.Name, path); c != (indexCached) {
		t.Errorf("rejected wildcard evicted layers: %+v", c)
	}
}

func TestEvictUnknownRemote(t *testing.T) {
	requireStack(t)
	var pe *ProxyError
	if err := testEngine.Evict(context.Background(), "evict-no-such-remote", "a/b"); !errors.As(err, &pe) || pe.Status != http.StatusNotFound {
		t.Fatalf("evict unknown remote = %v, want 404", err)
	}
}

func holdLock(t *testing.T, remote, path string) {
	t.Helper()
	ctx := context.Background()
	if ok, err := testCache.AcquireLock(ctx, remote, path, time.Minute); !ok || err != nil {
		t.Fatalf("acquire: %v %v", ok, err)
	}
	t.Cleanup(func() { _ = testCache.ReleaseLock(ctx, remote, path) })
}

func TestEvictWaitsForFetchLock(t *testing.T) {
	requireStack(t)
	srv, _, _ := changingUpstream(t)
	r := rpmRemote(t, "evict-lock", srv.URL)
	ctx := context.Background()
	const path = "8/Everything/x86_64/repodata/repomd.xml"
	holdLock(t, r.Name, path)
	done := make(chan error, 1)
	go func() { done <- testEngine.Evict(ctx, r.Name, path) }()
	select {
	case err := <-done:
		t.Fatalf("evict returned while fetch lock held: %v", err)
	case <-time.After(200 * time.Millisecond):
	}
	_ = testCache.ReleaseLock(ctx, r.Name, path)
	if err := <-done; err != nil {
		t.Fatalf("evict: %v", err)
	}
	ok, err := testCache.AcquireLock(ctx, r.Name, path, time.Second)
	if !ok || err != nil {
		t.Fatalf("lock still held after evict: %v %v", ok, err)
	}
}

func TestEvictCancelledWhileWaitingDeletesNothing(t *testing.T) {
	requireStack(t)
	srv, _, _ := changingUpstream(t)
	r := rpmRemote(t, "evict-cancel", srv.URL)
	const path = "8/Everything/x86_64/repodata/repomd.xml"
	fetchBody(t, r, path)
	holdLock(t, r.Name, path)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- testEngine.Evict(ctx, r.Name, path) }()
	time.Sleep(100 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("cancelled evict returned nil")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cancelled evict did not return")
	}
	if c := layers(t, r.Name, path); c != (indexCached) {
		t.Errorf("cancelled evict deleted layers: %+v", c)
	}
}

func TestEvictLockTimeoutIs503(t *testing.T) {
	requireStack(t)
	srv, _, _ := changingUpstream(t)
	r := rpmRemote(t, "evict-timeout", srv.URL)
	const path = "8/Everything/x86_64/repodata/repomd.xml"
	fetchBody(t, r, path)
	holdLock(t, r.Name, path)
	prev := testEngine.evictLockWait
	testEngine.evictLockWait = 100 * time.Millisecond
	t.Cleanup(func() { testEngine.evictLockWait = prev })

	var pe *ProxyError
	if err := testEngine.Evict(context.Background(), r.Name, path); !errors.As(err, &pe) || pe.Status != http.StatusServiceUnavailable {
		t.Fatalf("evict = %v, want 503", err)
	}
	if c := layers(t, r.Name, path); c != (indexCached) {
		t.Errorf("timed-out evict deleted layers: %+v", c)
	}
}

func TestEvictWildcardClearsPrefixOnly(t *testing.T) {
	requireStack(t)
	srv, rev, _ := changingUpstream(t)
	r := rpmRemote(t, "evict-wild", srv.URL)
	const (
		repomd = "8/Everything/x86_64/repodata/repomd.xml"
		rpm    = "8/Everything/x86_64/Packages/a/a-1.0-1.el8.x86_64.rpm"
		other  = "9/Everything/x86_64/repodata/repomd.xml"
	)
	for _, p := range []string{repomd, rpm, other} {
		fetchBody(t, r, p)
	}
	if c := layers(t, r.Name, rpm); c != blobCached {
		t.Fatalf("%s before evict = %+v, want %+v", rpm, c, blobCached)
	}
	rev.Store("rev2")

	if err := testEngine.Evict(context.Background(), r.Name, "8/Everything/x86_64/*"); err != nil {
		t.Fatalf("evict: %v", err)
	}
	for _, p := range []string{repomd, rpm} {
		if c := layers(t, r.Name, p); c != (cached{}) {
			t.Errorf("%s after evict = %+v, want every layer gone", p, c)
		}
	}
	if c := layers(t, r.Name, other); c != (indexCached) {
		t.Errorf("%s outside prefix = %+v, want %+v", other, c, indexCached)
	}
	if got := fetchBody(t, r, repomd); got != "rev2:/"+repomd {
		t.Errorf("index under prefix = %q, want rev2", got)
	}
	if got := fetchBody(t, r, rpm); got != "rev2:/"+rpm {
		t.Errorf("artifact under prefix = %q, want rev2", got)
	}
	if got := fetchBody(t, r, other); got != "rev1:/"+other {
		t.Errorf("path outside prefix = %q, want cached rev1", got)
	}
}
