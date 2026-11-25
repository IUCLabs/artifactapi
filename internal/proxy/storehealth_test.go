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
	mode  atomic.Int32
	calls atomic.Int64
	mu    sync.Mutex
	objs  map[string][]byte
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
			return
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		switch r.Method {
		case http.MethodPut:
			f.objs[key] = readS3Body(r)
			w.Header().Set("ETag", `"x"`)
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

	if !f.has(blobKey("immutable blob")) || !f.has(storage.IndexKey(idx.Name, "pkg")) {
		t.Error("write-behind did not upload once the store recovered")
	}
	if a, _ := testDB.GetArtifact(ctx, blob.Name, "blob.bin"); a == nil {
		t.Error("write-behind did not record the artifact")
	}
	if fresh, _ := testCache.CheckTTL(ctx, idx.Name, "pkg"); !fresh {
		t.Error("write-behind did not mark the index fresh")
	}
	if etag, _ := testCache.GetETag(ctx, idx.Name, "pkg"); etag != `"v1"` {
		t.Errorf("write-behind etag = %q", etag)
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
