package proxy

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/IUCLabs/artifactapi/internal/storage"
	"github.com/IUCLabs/artifactapi/pkg/models"
)

const (
	storeOK int32 = iota
	storeDown
	storeHang
)

// fakeStore is a minimal in-memory S3 that counts every request it receives.
type fakeStore struct {
	mode    atomic.Int32
	calls   atomic.Int64
	puts    atomic.Int64
	denyPut atomic.Bool
	failPut atomic.Bool
	failGet atomic.Bool // fails reads of stored objects
	putGate atomic.Pointer[chan struct{}]
	putWait atomic.Int64
	mu      sync.Mutex
	objs    map[string][]byte
}

func newFakeStore(t *testing.T) (*fakeStore, *storage.S3) {
	t.Helper()
	f := &fakeStore{objs: map[string][]byte{}}
	stop := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.calls.Add(1)
		switch f.mode.Load() {
		case storeHang:
			select {
			case <-r.Context().Done():
			case <-stop:
			}
			return
		case storeDown:
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		key := strings.TrimPrefix(r.URL.Path, "/bucket/")
		if key == r.URL.Path || key == "" {
			if r.Method == http.MethodGet && r.URL.Query().Has("list-type") {
				_, _ = io.WriteString(w, `<ListBucketResult><IsTruncated>false</IsTruncated></ListBucketResult>`)
			}
			return
		}
		if g := f.putGate.Load(); g != nil && r.Method == http.MethodPut {
			f.putWait.Add(1)
			<-*g
		}
		switch {
		case r.Method == http.MethodPut && f.denyPut.Load():
			f.puts.Add(1)
			w.WriteHeader(http.StatusForbidden)
			return
		case r.Method == http.MethodPut && f.failPut.Load():
			f.puts.Add(1)
			w.WriteHeader(http.StatusInternalServerError)
			return
		case r.Method != http.MethodPut && f.failGet.Load() && f.has(key):
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		switch r.Method {
		case http.MethodPut:
			f.puts.Add(1)
			f.objs[key] = readS3Body(r)
			w.Header().Set("ETag", `"x"`)
		case http.MethodDelete:
			delete(f.objs, key)
			w.WriteHeader(http.StatusNoContent)
		case http.MethodGet, http.MethodHead:
			b, ok := f.objs[key]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			w.Header().Set("Content-Length", fmt.Sprint(len(b)))
			w.Header().Set("Last-Modified", time.Now().UTC().Format(http.TimeFormat))
			w.Header().Set("ETag", `"x"`)
			if r.Method == http.MethodGet {
				_, _ = w.Write(b)
			}
		}
	}))
	t.Cleanup(func() { close(stop); srv.Close() })
	s3, err := storage.NewS3(strings.TrimPrefix(srv.URL, "http://"), "key", "secret", "bucket", false, "us-east-1")
	if err != nil {
		t.Fatal(err)
	}
	return f, s3
}

// readS3Body undoes minio-go's aws-chunked signing on plain-HTTP uploads.
func readS3Body(r *http.Request) []byte {
	if !strings.HasPrefix(r.Header.Get("X-Amz-Content-Sha256"), "STREAMING-") {
		b, _ := io.ReadAll(r.Body)
		return b
	}
	var out []byte
	br := bufio.NewReader(r.Body)
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			return out
		}
		n, err := strconv.ParseInt(strings.SplitN(strings.TrimSpace(line), ";", 2)[0], 16, 64)
		if err != nil || n == 0 {
			return out
		}
		chunk := make([]byte, n+2)
		if _, err := io.ReadFull(br, chunk); err != nil {
			return out
		}
		out = append(out, chunk[:n]...)
	}
}

func (f *fakeStore) has(key string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.objs[key]
	return ok
}

// unhealthyEngine returns an engine whose store has just been marked unhealthy by a failed call.
func unhealthyEngine(t *testing.T) (*Engine, *fakeStore, *storage.S3) {
	t.Helper()
	f, s3 := newFakeStore(t)
	eng := NewEngine(testDB, testCache, s3)
	f.mode.Store(storeDown)
	_, _ = eng.store.Stat(context.Background(), "x")
	if s3.Healthy() {
		t.Fatal("store still healthy after a failed call")
	}
	f.mode.Store(storeOK)
	f.calls.Store(0)
	return eng, f, s3
}

func isolateTempDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("TMPDIR", dir)
	return dir
}

func assertEmptyDir(t *testing.T, dir string) {
	t.Helper()
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Errorf("temp files left behind: %v", entries)
	}
}

func blobKey(content string) string {
	h := sha256.Sum256([]byte(content))
	return storage.BlobKey(hex.EncodeToString(h[:]))
}

func TestUnhealthyStoreServesWithoutTouchingIt(t *testing.T) {
	requireStack(t)
	ctx := context.Background()
	tmp := isolateTempDir(t)
	eng, f, _ := unhealthyEngine(t)
	eng.writeBehindBudget = 200 * time.Millisecond

	blob := seed(t, genericRemote("sh-fast-blob"))
	res, err := eng.Fetch(ctx, blob, "blob.bin", prov(t, models.PackageGeneric))
	if err != nil {
		t.Fatal(err)
	}
	if body := readAll(t, res); body != "immutable blob" || res.Size != int64(len(body)) {
		t.Errorf("blob: size=%d body=%q", res.Size, body)
	}
	idx := seed(t, models.Remote{Name: "sh-fast-npm", PackageType: models.PackageNPM, RepoType: models.RepoTypeRemote, BaseURL: upstream.URL, CheckMutable: true, MutableTTL: 60})
	res, err = eng.Fetch(ctx, idx, "pkg", prov(t, models.PackageNPM))
	if err != nil {
		t.Fatal(err)
	}
	if body := readAll(t, res); !strings.Contains(body, `"name":"pkg"`) {
		t.Errorf("index body %q", body)
	}
	_, _ = eng.Head(ctx, idx, "pkg", prov(t, models.PackageNPM))

	eng.writeBehindWG.Wait()
	if n := f.calls.Load(); n != 0 {
		t.Errorf("store calls while unhealthy = %d, want 0", n)
	}
	assertEmptyDir(t, tmp)
	if a, _ := testDB.GetArtifact(ctx, blob.Name, "blob.bin"); a != nil {
		t.Error("a dropped write-behind must not leave an artifact row")
	}
	if fresh, _ := testCache.CheckTTL(ctx, idx.Name, "pkg"); fresh {
		t.Error("a dropped write-behind must not mark the index fresh")
	}
}

func TestWriteBehindCachesWhenStoreRecovers(t *testing.T) {
	requireStack(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tmp := isolateTempDir(t)
	eng, f, s3 := unhealthyEngine(t)

	blob := seed(t, genericRemote("sh-recover-blob"))
	res, err := eng.Fetch(ctx, blob, "blob.bin", prov(t, models.PackageGeneric))
	if err != nil {
		t.Fatal(err)
	}
	_ = readAll(t, res)
	idx := seed(t, models.Remote{Name: "sh-recover-npm", PackageType: models.PackageNPM, RepoType: models.RepoTypeRemote, BaseURL: upstream.URL, CheckMutable: true, MutableTTL: 60})
	res, err = eng.Fetch(ctx, idx, "pkg", prov(t, models.PackageNPM))
	if err != nil {
		t.Fatal(err)
	}
	_ = readAll(t, res)

	time.Sleep(3 * writeBehindPoll)
	go s3.RunHealthProbe(ctx, 50*time.Millisecond, time.Second)
	eng.writeBehindWG.Wait()

	if !f.has(blobKey("immutable blob")) {
		t.Error("write-behind did not upload once the store recovered")
	}
	if a, _ := testDB.GetArtifact(ctx, blob.Name, "blob.bin"); a == nil {
		t.Error("write-behind did not record the artifact")
	}
	if fresh, _ := testCache.CheckTTL(ctx, idx.Name, "pkg"); fresh || f.has(storage.IndexKey(idx.Name, "pkg")) {
		t.Error("an index served while unhealthy must stay uncached")
	}
	assertEmptyDir(t, tmp)

	res, err = eng.Fetch(ctx, blob, "blob.bin", prov(t, models.PackageGeneric))
	if err != nil {
		t.Fatal(err)
	}
	if body := readAll(t, res); res.Source != "cache" || body != "immutable blob" {
		t.Errorf("next fetch: source=%s body=%q", res.Source, body)
	}
}

func TestWriteBehindSkipsWhenSlotsFull(t *testing.T) {
	requireStack(t)
	ctx := context.Background()
	tmp := isolateTempDir(t)
	eng, _, _ := unhealthyEngine(t)
	for range cap(eng.writeBehind) {
		eng.writeBehind <- struct{}{}
	}

	blob := seed(t, genericRemote("sh-full-blob"))
	res, err := eng.Fetch(ctx, blob, "blob.bin", prov(t, models.PackageGeneric))
	if err != nil {
		t.Fatal(err)
	}
	if body := readAll(t, res); body != "immutable blob" {
		t.Errorf("body %q", body)
	}
	assertEmptyDir(t, tmp)
	if n := len(eng.writeBehind); n != cap(eng.writeBehind) {
		t.Errorf("slots in use = %d, want %d", n, cap(eng.writeBehind))
	}
}

func TestUnhealthySpoolRemovedOnBrokenStreams(t *testing.T) {
	requireStack(t)
	ctx := context.Background()
	tmp := isolateTempDir(t)
	eng, _, _ := unhealthyEngine(t)
	blob := seed(t, genericRemote("sh-broken-blob"))

	res, err := eng.Fetch(ctx, blob, "blob.bin", prov(t, models.PackageGeneric))
	if err != nil {
		t.Fatal(err)
	}
	_, _ = res.Reader.Read(make([]byte, 3))
	_ = res.Reader.Close()

	res, err = eng.Fetch(ctx, blob, "truncated.bin", prov(t, models.PackageGeneric))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadAll(res.Reader); err == nil {
		t.Error("truncated upstream read succeeded")
	}
	_ = res.Reader.Close()

	if len(eng.writeBehind) != 0 {
		t.Error("a broken stream started a write-behind")
	}
	assertEmptyDir(t, tmp)
	if a, _ := testDB.GetArtifact(ctx, blob.Name, "blob.bin"); a != nil {
		t.Error("a disconnected download must not be cached")
	}
}

// A store that accepts connections and never answers costs a miss about one request timeout,
// then flips the store unhealthy so the next miss skips it.
func TestHungStorePassesThroughWithinRequestTimeout(t *testing.T) {
	requireStack(t)
	ctx := context.Background()
	isolateTempDir(t)
	f, s3 := newFakeStore(t)
	eng := NewEngine(testDB, testCache, s3)
	eng.writeBehindBudget = 100 * time.Millisecond
	f.mode.Store(storeHang)

	blob := seed(t, genericRemote("sh-hung-blob"))
	start := time.Now()
	res, err := eng.Fetch(ctx, blob, "blob.bin", prov(t, models.PackageGeneric))
	if err != nil {
		t.Fatal(err)
	}
	if body := readAll(t, res); body != "immutable blob" {
		t.Errorf("body %q", body)
	}
	if elapsed := time.Since(start); elapsed > storage.RequestTimeout+time.Second {
		t.Errorf("blob miss on a hung store took %s", elapsed)
	}
	if s3.Healthy() {
		t.Error("a timed-out store call must mark the store unhealthy")
	}

	f.calls.Store(0)
	start = time.Now()
	idx := seed(t, models.Remote{Name: "sh-hung-npm", PackageType: models.PackageNPM, RepoType: models.RepoTypeRemote, BaseURL: upstream.URL})
	res, err = eng.Fetch(ctx, idx, "pkg", prov(t, models.PackageNPM))
	if err != nil {
		t.Fatal(err)
	}
	_ = readAll(t, res)
	if elapsed := time.Since(start); elapsed > time.Second || f.calls.Load() != 0 {
		t.Errorf("index miss after the store went unhealthy took %s with %d store calls", elapsed, f.calls.Load())
	}
	eng.writeBehindWG.Wait()
}

func TestHungStoreCallerCancelStaysAnError(t *testing.T) {
	requireStack(t)
	isolateTempDir(t)
	f, s3 := newFakeStore(t)
	eng := NewEngine(testDB, testCache, s3)
	f.mode.Store(storeHang)

	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(200*time.Millisecond, cancel)
	blob := seed(t, genericRemote("sh-cancel-blob"))
	res, err := eng.Fetch(ctx, blob, "blob.bin", prov(t, models.PackageGeneric))
	if !errors.Is(err, context.Canceled) || errors.Is(err, storage.ErrStoreUnavailable) || res != nil {
		t.Fatalf("want a cancellation error and no result, got res=%v err=%v", res, err)
	}
	if !s3.Healthy() {
		t.Error("the caller's cancellation must not mark the store unhealthy")
	}
}

// fetchAndDrain fetches path and reads it to the end, which queues a write-behind on an unhealthy store.
func fetchAndDrain(t *testing.T, eng *Engine, remote models.Remote, path string) {
	t.Helper()
	res, err := eng.Fetch(context.Background(), remote, path, prov(t, models.PackageGeneric))
	if err != nil {
		t.Fatal(err)
	}
	if body := readAll(t, res); body != "immutable blob" {
		t.Fatalf("body %q", body)
	}
}

func TestWriteBehindSkipsEvictedPath(t *testing.T) {
	requireStack(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tmp := isolateTempDir(t)
	eng, f, s3 := unhealthyEngine(t)

	blob := seed(t, genericRemote("sh-evict-blob"))
	fetchAndDrain(t, eng, blob, "blob.bin")
	if err := eng.Evict(ctx, blob.Name, "blob.bin"); err != nil {
		t.Fatal(err)
	}
	go s3.RunHealthProbe(ctx, 50*time.Millisecond, time.Second)
	eng.writeBehindWG.Wait()

	if f.has(blobKey("immutable blob")) {
		t.Error("write-behind uploaded a path evicted during its wait")
	}
	if a, _ := testDB.GetArtifact(ctx, blob.Name, "blob.bin"); a != nil {
		t.Error("write-behind restored the artifact row of an evicted path")
	}
	assertEmptyDir(t, tmp)
}

func TestWriteBehindDedupesPath(t *testing.T) {
	requireStack(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tmp := isolateTempDir(t)
	eng, f, s3 := unhealthyEngine(t)

	blob := seed(t, genericRemote("sh-dedupe-blob"))
	for range 3 {
		fetchAndDrain(t, eng, blob, "blob.bin")
	}
	if n := len(eng.writeBehind); n != 1 {
		t.Errorf("slots in use for one path = %d, want 1", n)
	}
	if entries, _ := os.ReadDir(tmp); len(entries) != 1 {
		t.Errorf("spools kept for one path = %d, want 1", len(entries))
	}

	go s3.RunHealthProbe(ctx, 50*time.Millisecond, time.Second)
	eng.writeBehindWG.Wait()
	if n := f.puts.Load(); n != 1 {
		t.Errorf("uploads for one path = %d, want 1", n)
	}
	assertEmptyDir(t, tmp)
}

func TestWriteBehindStopsOnClientError(t *testing.T) {
	requireStack(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	isolateTempDir(t)
	eng, f, s3 := unhealthyEngine(t)
	eng.writeBehindBudget = 10 * time.Second
	f.denyPut.Store(true)

	blob := seed(t, genericRemote("sh-deny-blob"))
	fetchAndDrain(t, eng, blob, "blob.bin")
	go s3.RunHealthProbe(ctx, 50*time.Millisecond, time.Second)
	start := time.Now()
	eng.writeBehindWG.Wait()

	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("write-behind kept retrying a 403 for %s", elapsed)
	}
	if n := f.puts.Load(); n != 1 {
		t.Errorf("uploads after a 403 = %d, want 1", n)
	}
	if !s3.Healthy() {
		t.Error("a 403 must not mark the store unhealthy")
	}
}

func TestDownloadFailureAfterPutServesSpool(t *testing.T) {
	requireStack(t)
	ctx := context.Background()
	tmp := isolateTempDir(t)
	f, s3 := newFakeStore(t)
	eng := NewEngine(testDB, testCache, s3)
	f.failGet.Store(true)

	blob := seed(t, genericRemote("sh-getfail-blob"))
	res, err := eng.Fetch(ctx, blob, "blob.bin", prov(t, models.PackageGeneric))
	if err != nil {
		t.Fatalf("a failed read-back after a stored put must still serve: %v", err)
	}
	if body := readAll(t, res); body != "immutable blob" || res.Size != int64(len(body)) || res.Source != "remote" {
		t.Errorf("blob: source=%s size=%d body=%q", res.Source, res.Size, body)
	}
	if !f.has(blobKey("immutable blob")) {
		t.Error("blob was not stored")
	}
	assertEmptyDir(t, tmp)
}

func TestUnhealthyStoreNeverUploads(t *testing.T) {
	requireStack(t)
	tmp := isolateTempDir(t)
	eng, f, _ := unhealthyEngine(t)
	eng.writeBehindBudget = 300 * time.Millisecond

	blob := seed(t, genericRemote("sh-noput-blob"))
	fetchAndDrain(t, eng, blob, "blob.bin")
	eng.writeBehindWG.Wait()

	if n := f.puts.Load(); n != 0 {
		t.Errorf("PUTs while unhealthy = %d, want 0", n)
	}
	assertEmptyDir(t, tmp)
}

func TestFailedUploadKeepsStoreHealthy(t *testing.T) {
	requireStack(t)
	tmp := isolateTempDir(t)
	f, s3 := newFakeStore(t)
	eng := NewEngine(testDB, testCache, s3)
	f.failPut.Store(true)

	eng.writeBehindBudget = 300 * time.Millisecond

	blob := seed(t, genericRemote("sh-putfail-blob"))
	fetchAndDrain(t, eng, blob, "blob.bin")
	eng.writeBehindWG.Wait()

	if f.puts.Load() == 0 {
		t.Fatal("no upload was attempted on a healthy store")
	}
	if !s3.Healthy() {
		t.Error("a failed upload must not mark the store unhealthy")
	}
	assertEmptyDir(t, tmp)
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestEvictOnDownStoreStillStopsWriteBehind(t *testing.T) {
	requireStack(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tmp := isolateTempDir(t)
	eng, f, s3 := unhealthyEngine(t)

	blob := seed(t, genericRemote("sh-evictdown-blob"))
	fetchAndDrain(t, eng, blob, "blob.bin")
	f.mode.Store(storeDown)
	if err := eng.Evict(ctx, blob.Name, "blob.bin"); err == nil {
		t.Fatal("evict on a down store reported success")
	}
	f.mode.Store(storeOK)
	go s3.RunHealthProbe(ctx, 50*time.Millisecond, time.Second)
	eng.writeBehindWG.Wait()

	if f.has(blobKey("immutable blob")) {
		t.Error("write-behind uploaded a path evicted while the store was down")
	}
	if a, _ := testDB.GetArtifact(ctx, blob.Name, "blob.bin"); a != nil {
		t.Error("write-behind restored the artifact row of a path evicted while the store was down")
	}
	assertEmptyDir(t, tmp)
}

func TestWriteBehindSkipsPathEvictedOnAnotherReplica(t *testing.T) {
	requireStack(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tmp := isolateTempDir(t)
	engA, f, s3 := unhealthyEngine(t)
	engB := NewEngine(testDB, testCache, s3)

	blob := seed(t, genericRemote("sh-replica-blob"))
	fetchAndDrain(t, engA, blob, "blob.bin")
	if err := engB.Evict(ctx, blob.Name, "blob.bin"); err != nil {
		t.Fatal(err)
	}
	go s3.RunHealthProbe(ctx, 50*time.Millisecond, time.Second)
	engA.writeBehindWG.Wait()

	if f.has(blobKey("immutable blob")) {
		t.Error("write-behind uploaded a path evicted on another replica")
	}
	if a, _ := testDB.GetArtifact(ctx, blob.Name, "blob.bin"); a != nil {
		t.Error("write-behind restored a path evicted on another replica")
	}
	assertEmptyDir(t, tmp)
}

func TestWriteBehindWaitsForHeldFetchLock(t *testing.T) {
	requireStack(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tmp := isolateTempDir(t)
	eng, f, s3 := unhealthyEngine(t)

	blob := seed(t, genericRemote("sh-lockwait-blob"))
	fetchAndDrain(t, eng, blob, "blob.bin")
	lock, err := testCache.AcquireLock(ctx, blob.Name, "blob.bin", time.Minute)
	if err != nil || lock == "" {
		t.Fatalf("take lock: %q %v", lock, err)
	}
	go s3.RunHealthProbe(ctx, 50*time.Millisecond, time.Second)
	waitFor(t, "store healthy", s3.Healthy)
	time.Sleep(4 * writeBehindPoll)
	if n := f.puts.Load(); n != 0 {
		t.Fatalf("write-behind uploaded under a held fetch lock (%d PUTs)", n)
	}
	_ = testCache.ReleaseLock(ctx, blob.Name, "blob.bin", lock)
	eng.writeBehindWG.Wait()

	if a, _ := testDB.GetArtifact(ctx, blob.Name, "blob.bin"); a == nil || !f.has(blobKey("immutable blob")) {
		t.Error("write-behind did not cache once the fetch lock was released")
	}
	assertEmptyDir(t, tmp)
}

func TestWriteBehindDropsAtBudgetUnderHeldFetchLock(t *testing.T) {
	requireStack(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tmp := isolateTempDir(t)
	eng, f, s3 := unhealthyEngine(t)
	eng.writeBehindBudget = time.Second

	blob := seed(t, genericRemote("sh-lockdrop-blob"))
	lock, err := testCache.AcquireLock(ctx, blob.Name, "blob.bin", time.Minute)
	if err != nil || lock == "" {
		t.Fatalf("take lock: %q %v", lock, err)
	}
	defer func() { _ = testCache.ReleaseLock(ctx, blob.Name, "blob.bin", lock) }()
	fetchAndDrain(t, eng, blob, "blob.bin")
	go s3.RunHealthProbe(ctx, 50*time.Millisecond, time.Second)
	start := time.Now()
	eng.writeBehindWG.Wait()

	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Errorf("write-behind outlived its budget under a held lock: %s", elapsed)
	}
	if n := f.puts.Load(); n != 0 {
		t.Errorf("PUTs under a held fetch lock = %d, want 0", n)
	}
	if a, _ := testDB.GetArtifact(ctx, blob.Name, "blob.bin"); a != nil {
		t.Error("a dropped write-behind left an artifact row")
	}
	assertEmptyDir(t, tmp)
}

func TestFetchWaitsForWriteBehindHoldingLock(t *testing.T) {
	requireStack(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	isolateTempDir(t)
	eng, f, s3 := unhealthyEngine(t)
	gate := make(chan struct{})
	f.putGate.Store(&gate)

	blob := seed(t, genericRemote("sh-wblock-blob"))
	fetchAndDrain(t, eng, blob, "blob.bin")
	go s3.RunHealthProbe(ctx, 50*time.Millisecond, time.Second)
	waitFor(t, "write-behind upload", func() bool { return f.putWait.Load() > 0 })
	time.AfterFunc(300*time.Millisecond, func() { close(gate) })

	res, err := eng.Fetch(ctx, blob, "blob.bin", prov(t, models.PackageGeneric))
	if err != nil {
		t.Fatal(err)
	}
	if body := readAll(t, res); res.Source != "cache" || body != "immutable blob" {
		t.Errorf("fetch during write-behind: source=%s body=%q", res.Source, body)
	}
	eng.writeBehindWG.Wait()
	if n := f.puts.Load(); n != 1 {
		t.Errorf("uploads = %d, want 1", n)
	}
}

func TestStampedeSpoolsOnce(t *testing.T) {
	requireStack(t)
	tmp := isolateTempDir(t)
	eng, _, _ := unhealthyEngine(t)
	eng.writeBehindBudget = 200 * time.Millisecond

	blob := seed(t, genericRemote("sh-stampede-blob"))
	var open []*FetchResult
	for range 3 {
		res, err := eng.Fetch(context.Background(), blob, "blob.bin", prov(t, models.PackageGeneric))
		if err != nil {
			t.Fatal(err)
		}
		open = append(open, res)
	}
	if entries, _ := os.ReadDir(tmp); len(entries) != 1 {
		t.Errorf("spools for concurrent misses on one path = %d, want 1", len(entries))
	}
	for _, res := range open {
		if body := readAll(t, res); body != "immutable blob" {
			t.Errorf("body %q", body)
		}
	}
	eng.writeBehindWG.Wait()
	assertEmptyDir(t, tmp)
}

func TestCloseCancelsWriteBehind(t *testing.T) {
	requireStack(t)
	tmp := isolateTempDir(t)
	eng, f, _ := unhealthyEngine(t)

	blob := seed(t, genericRemote("sh-close-blob"))
	fetchAndDrain(t, eng, blob, "blob.bin")
	done := make(chan struct{})
	go func() { eng.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Close did not cancel a pending write-behind")
	}
	assertEmptyDir(t, tmp)

	fetchAndDrain(t, eng, blob, "blob.bin")
	assertEmptyDir(t, tmp)
	if n := f.puts.Load(); n != 0 {
		t.Errorf("PUTs after close = %d, want 0", n)
	}
}

func TestCloseWithOpenReader(t *testing.T) {
	requireStack(t)
	tmp := isolateTempDir(t)
	eng, f, _ := unhealthyEngine(t)

	blob := seed(t, genericRemote("sh-closeopen-blob"))
	res, err := eng.Fetch(context.Background(), blob, "blob.bin", prov(t, models.PackageGeneric))
	if err != nil {
		t.Fatal(err)
	}
	eng.Close()
	if body := readAll(t, res); body != "immutable blob" {
		t.Errorf("body %q", body)
	}
	eng.writeBehindWG.Wait()

	if n := f.puts.Load(); n != 0 {
		t.Errorf("PUTs after close = %d, want 0", n)
	}
	if n := len(eng.writeBehind); n != 0 {
		t.Errorf("slots in use after close = %d, want 0", n)
	}
	if n := len(eng.pending); n != 0 {
		t.Errorf("pending paths after close = %d, want 0", n)
	}
	assertEmptyDir(t, tmp)
}

func TestSpoolsAcrossPathsStayWithinSlots(t *testing.T) {
	requireStack(t)
	tmp := isolateTempDir(t)
	eng, _, _ := unhealthyEngine(t)
	eng.writeBehindBudget = 200 * time.Millisecond

	var open []*FetchResult
	for i := range 10 {
		blob := seed(t, genericRemote(fmt.Sprintf("sh-slots-blob-%d", i)))
		res, err := eng.Fetch(context.Background(), blob, "blob.bin", prov(t, models.PackageGeneric))
		if err != nil {
			t.Fatal(err)
		}
		open = append(open, res)
	}
	if entries, _ := os.ReadDir(tmp); len(entries) > cap(eng.writeBehind) {
		t.Errorf("spools for %d open misses = %d, want at most %d", len(open), len(entries), cap(eng.writeBehind))
	}
	for _, res := range open {
		if body := readAll(t, res); body != "immutable blob" {
			t.Errorf("body %q", body)
		}
	}
	eng.writeBehindWG.Wait()
	assertEmptyDir(t, tmp)
	if n := len(eng.writeBehind); n != 0 {
		t.Errorf("slots in use = %d, want 0", n)
	}
}

func TestFailedUploadSpoolCachedLater(t *testing.T) {
	requireStack(t)
	ctx := context.Background()
	tmp := isolateTempDir(t)
	f, s3 := newFakeStore(t)
	eng := NewEngine(testDB, testCache, s3)
	f.failPut.Store(true)

	blob := seed(t, genericRemote("sh-putlater-blob"))
	fetchAndDrain(t, eng, blob, "blob.bin")
	f.failPut.Store(false)
	eng.writeBehindWG.Wait()

	if a, _ := testDB.GetArtifact(ctx, blob.Name, "blob.bin"); a == nil || !f.has(blobKey("immutable blob")) {
		t.Error("the spool of a refused upload was not cached afterwards")
	}
	assertEmptyDir(t, tmp)
}

func TestFailedUploadSpoolDroppedWhenSlotsFull(t *testing.T) {
	requireStack(t)
	tmp := isolateTempDir(t)
	f, s3 := newFakeStore(t)
	eng := NewEngine(testDB, testCache, s3)
	f.failPut.Store(true)
	for range cap(eng.writeBehind) {
		eng.writeBehind <- struct{}{}
	}

	blob := seed(t, genericRemote("sh-putfull-blob"))
	fetchAndDrain(t, eng, blob, "blob.bin")
	assertEmptyDir(t, tmp)
	if n := len(eng.pending); n != 0 {
		t.Errorf("pending paths = %d, want 0", n)
	}
}

// lockWaiters counts sessions blocked on a lock in a statement matching like.
func lockWaiters(t *testing.T, like string) int {
	t.Helper()
	var n int
	err := testDB.Pool.QueryRow(context.Background(),
		`SELECT count(*) FROM pg_stat_activity WHERE wait_event_type = 'Lock' AND query LIKE $1`, like).Scan(&n)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func TestWildcardEvictDuringWriteBehind(t *testing.T) {
	requireStack(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tmp := isolateTempDir(t)
	eng, f, s3 := unhealthyEngine(t)

	blob := seed(t, genericRemote("sh-wildevict-blob"))
	fetchAndDrain(t, eng, blob, "dir/blob.bin")
	token, err := testCache.PendingToken(ctx, blob.Name, "dir/blob.bin", time.Hour)
	if err != nil {
		t.Fatal(err)
	}

	tx, err := testDB.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `LOCK TABLE artifacts IN SHARE ROW EXCLUSIVE MODE`); err != nil {
		t.Fatal(err)
	}
	go s3.RunHealthProbe(ctx, 50*time.Millisecond, time.Second)
	waitFor(t, "write-behind blocked recording", func() bool { return lockWaiters(t, "%INSERT INTO artifacts%") > 0 })

	evicted := make(chan error, 1)
	go func() { evicted <- eng.Evict(ctx, blob.Name, "dir/*") }()
	waitFor(t, "evict blocked deleting", func() bool { return lockWaiters(t, "%DELETE FROM artifacts%") > 0 })
	if valid, _ := testCache.PendingValid(ctx, blob.Name, "dir/blob.bin", token); valid {
		t.Error("evict deleted rows before dropping the pending token")
	}
	_ = tx.Rollback(ctx)
	if err := <-evicted; err != nil {
		t.Fatal(err)
	}
	eng.writeBehindWG.Wait()

	if a, _ := testDB.GetArtifact(ctx, blob.Name, "dir/blob.bin"); a != nil {
		t.Error("write-behind restored the artifact row of a wildcard-evicted path")
	}
	if fresh, _ := testCache.CheckTTL(ctx, blob.Name, "dir/blob.bin"); fresh {
		t.Error("write-behind left a TTL on a wildcard-evicted path")
	}
	if f.puts.Load() == 0 {
		t.Error("write-behind never reached the store")
	}
	assertEmptyDir(t, tmp)
}

func TestUnhealthyMissStreamsWhenSpoolFails(t *testing.T) {
	requireStack(t)
	eng, _, _ := unhealthyEngine(t)
	t.Setenv("TMPDIR", filepath.Join(t.TempDir(), "missing"))

	blob := seed(t, genericRemote("sh-nospool-blob"))
	fetchAndDrain(t, eng, blob, "blob.bin")
	if n, p := len(eng.writeBehind), len(eng.pending); n != 0 || p != 0 {
		t.Errorf("slots=%d pending=%d after a failed spool, want 0", n, p)
	}
}
