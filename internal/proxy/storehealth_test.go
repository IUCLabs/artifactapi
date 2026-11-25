package proxy

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/IUCLabs/artifactapi/internal/storage"
	"github.com/IUCLabs/artifactapi/pkg/models"
	goredis "github.com/redis/go-redis/v9"
)

func TestHysteresis(t *testing.T) {
	fail := errors.New("probe failed")
	steps := func(h *hysteresis, results ...error) {
		for _, r := range results {
			h.observe(r)
		}
	}

	h := &hysteresis{cfg: DefaultProbeConfig()}
	steps(h, nil, nil)
	if h.known {
		t.Fatal("state settled before the up threshold")
	}
	steps(h, nil)
	if !h.known || !h.up {
		t.Fatal("3 successes must settle up")
	}
	steps(h, fail, fail)
	if !h.up {
		t.Fatal("2 failures must stay up")
	}
	steps(h, fail)
	if h.up {
		t.Fatal("the 3rd failure must flip down")
	}
	steps(h, nil, nil)
	if h.up {
		t.Fatal("2 successes must stay down")
	}
	steps(h, nil)
	if !h.up {
		t.Fatal("the 3rd success must flip up")
	}
	steps(h, fail, fail, nil, fail, fail)
	if !h.up {
		t.Fatal("a success mid-run must reset the failure count")
	}

	h = &hysteresis{cfg: ProbeConfig{UpThreshold: 1, DownThreshold: 5}}
	steps(h, fail, fail, fail, fail)
	if h.known {
		t.Fatal("4 failures settled a down threshold of 5")
	}
	steps(h, fail)
	if !h.known || h.up {
		t.Fatal("the 5th failure must settle down")
	}
	steps(h, nil)
	if !h.up {
		t.Fatal("an up threshold of 1 must flip on one success")
	}
}

func testRedisClient(t *testing.T) *goredis.Client {
	t.Helper()
	c := goredis.NewClient(must(goredis.ParseURL(testRedis)))
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func clearStoreHealth(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	_ = testRedisClient(t).Del(ctx, "s3:health").Err()
	t.Cleanup(func() { _ = testRedisClient(t).Del(ctx, "s3:health").Err() })
}

func storeHealthKey(t *testing.T) (string, time.Duration) {
	t.Helper()
	c := testRedisClient(t)
	val, _ := c.Get(context.Background(), "s3:health").Result()
	return val, c.PTTL(context.Background(), "s3:health").Val()
}

func setStoreUp(t *testing.T, up bool) {
	t.Helper()
	if err := testCache.SetStoreHealth(context.Background(), up, time.Minute); err != nil {
		t.Fatal(err)
	}
}

func TestRunStoreProbe(t *testing.T) {
	requireStack(t)
	clearStoreHealth(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	called := make(chan struct{})
	results := make(chan error)
	probe := func(context.Context) error {
		called <- struct{}{}
		return <-results
	}
	cfg := ProbeConfig{UpThreshold: 2, DownThreshold: 2, Interval: 200 * time.Millisecond, Timeout: time.Second}
	go RunStoreProbe(ctx, cfg, probe, testCache)
	<-called

	fail := errors.New("probe failed")
	step := func(err error, want string) {
		t.Helper()
		results <- err
		<-called
		val, ttl := storeHealthKey(t)
		if val != want {
			t.Fatalf("s3:health = %q, want %q", val, want)
		}
		if want != "" && (ttl <= 0 || ttl > 3*cfg.Interval) {
			t.Fatalf("s3:health TTL = %s, want (0, %s]", ttl, 3*cfg.Interval)
		}
	}
	step(fail, "")
	step(fail, "down")
	step(nil, "down")
	step(nil, "up")
	step(fail, "up")
	step(nil, "up")
	cancel()
	results <- nil
}

// faultyStore proxies a real MinIO, counting requests, and can hang or fail them.
type faultyStore struct {
	mode     atomic.Int32
	requests atomic.Int64
	putDelay time.Duration
}

const (
	storePass int32 = iota
	storeHang
	storeFailPut
	storeSlowPut
)

func newFaultyStore(t *testing.T, bucket string) (*faultyStore, *storage.S3) {
	t.Helper()
	fs := &faultyStore{putDelay: storage.RequestTimeout + 500*time.Millisecond}
	target, _ := url.Parse("http://" + testMinio.Endpoint)
	rp := httputil.NewSingleHostReverseProxy(target)
	stop := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fs.requests.Add(1)
		switch mode := fs.mode.Load(); {
		case mode == storeHang:
			select {
			case <-r.Context().Done():
			case <-stop:
			}
			return
		case mode == storeFailPut && r.Method == http.MethodPut:
			_, _ = io.Copy(io.Discard, r.Body)
			w.WriteHeader(http.StatusInternalServerError)
			return
		case mode == storeSlowPut && r.Method == http.MethodPut:
			time.Sleep(fs.putDelay)
		}
		rp.ServeHTTP(w, r)
	}))
	t.Cleanup(func() { close(stop); srv.Close() })
	s3, err := storage.NewS3(strings.TrimPrefix(srv.URL, "http://"), testMinio.AccessKey, testMinio.SecretKey, bucket, false, "")
	if err != nil {
		t.Fatal(err)
	}
	fs.requests.Store(0)
	return fs, s3
}

func assertEmptyDir(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		t.Errorf("temp file left in TMPDIR: %s", e.Name())
	}
}

func TestStoreKeyDownStreamsWithoutStore(t *testing.T) {
	requireStack(t)
	clearStoreHealth(t)
	ctx := context.Background()
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	fs, s3 := newFaultyStore(t, "proxy-keydown")
	eng := NewEngine(testDB, testCache, s3)
	setStoreUp(t, false)

	idx := seed(t, models.Remote{Name: "keydown-npm", PackageType: models.PackageNPM, RepoType: models.RepoTypeRemote, BaseURL: upstream.URL, CheckMutable: true})
	res, err := eng.Fetch(ctx, idx, "pkg", prov(t, models.PackageNPM))
	if err != nil {
		t.Fatalf("index fetch with key down: %v", err)
	}
	if body := readAll(t, res); res.Source != "remote" || !strings.Contains(body, `"name":"pkg"`) {
		t.Errorf("index: source=%s body=%q", res.Source, body)
	}
	if fresh, _ := testCache.CheckTTL(ctx, idx.Name, "pkg"); fresh {
		t.Error("an index served with the key down must get no TTL")
	}
	if etag, _ := testCache.GetETag(ctx, idx.Name, "pkg"); etag != "" {
		t.Errorf("an index served with the key down must get no ETag, got %q", etag)
	}

	blob := seed(t, genericRemote("keydown-blob"))
	_ = testCache.SetTTL(ctx, blob.Name, "blob.bin", time.Minute)
	res, err = eng.Fetch(ctx, blob, "blob.bin", prov(t, models.PackageGeneric))
	if err != nil {
		t.Fatalf("blob fetch with key down: %v", err)
	}
	if body := readAll(t, res); res.Source != "remote" || body != "immutable blob" {
		t.Errorf("blob: source=%s body=%q", res.Source, body)
	}
	if a, _ := testDB.GetArtifact(ctx, blob.Name, "blob.bin"); a != nil {
		t.Error("a blob served with the key down must not be recorded")
	}

	if _, err := eng.Head(ctx, blob, "blob.bin", prov(t, models.PackageGeneric)); err != nil {
		t.Fatalf("head with key down: %v", err)
	}

	if n := fs.requests.Load(); n != 0 {
		t.Errorf("S3 requests with the key down = %d, want 0", n)
	}
	assertEmptyDir(t, tmp)

	q := quarantineRemote(t, "keydown-quarantine", true)
	_, err = eng.Fetch(ctx, q, "fresh.tgz", prov(t, models.PackageNPM))
	_ = wantQuarantined(t, err)

	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := eng.Fetch(cctx, blob, "blob.bin", prov(t, models.PackageGeneric)); err == nil {
		t.Error("a cancelled request must fail with the key down")
	}

	setStoreUp(t, true)
	_ = testCache.ForgetPath(ctx, blob.Name, "blob.bin")
	res, err = eng.Fetch(ctx, blob, "blob.bin", prov(t, models.PackageGeneric))
	if err != nil {
		t.Fatalf("blob fetch after recovery: %v", err)
	}
	_ = readAll(t, res)
	res, err = eng.Fetch(ctx, blob, "blob.bin", prov(t, models.PackageGeneric))
	if err != nil {
		t.Fatalf("second blob fetch after recovery: %v", err)
	}
	if body := readAll(t, res); res.Source != "cache" || body != "immutable blob" {
		t.Errorf("after recovery: source=%s body=%q, want a cache hit", res.Source, body)
	}
}

func TestHungStoreFailsFastWithKeyUp(t *testing.T) {
	requireStack(t)
	clearStoreHealth(t)
	ctx := context.Background()
	fs, s3 := newFaultyStore(t, "proxy-hung")
	eng := NewEngine(testDB, testCache, s3)
	setStoreUp(t, true)
	fs.mode.Store(storeHang)

	blob := seed(t, genericRemote("hung-blob"))
	start := time.Now()
	res, err := eng.Fetch(ctx, blob, "blob.bin", prov(t, models.PackageGeneric))
	if err != nil {
		t.Fatalf("fetch on a hung store: %v", err)
	}
	if body := readAll(t, res); body != "immutable blob" {
		t.Errorf("body = %q", body)
	}
	if elapsed := time.Since(start); elapsed > storage.RequestTimeout+time.Second {
		t.Errorf("fetch on a hung store took %s, want about %s", elapsed, storage.RequestTimeout)
	}
	if val, _ := storeHealthKey(t); val != "up" {
		t.Errorf("s3:health = %q after a request-path failure, want unchanged up", val)
	}
}

func TestUploadOutcomeKeepsHealth(t *testing.T) {
	requireStack(t)
	clearStoreHealth(t)
	ctx := context.Background()
	fs, s3 := newFaultyStore(t, "proxy-upload")
	eng := NewEngine(testDB, testCache, s3)
	setStoreUp(t, true)
	p := prov(t, models.PackageGeneric)

	fs.mode.Store(storeSlowPut)
	slow := seed(t, genericRemote("upload-slow"))
	res, err := eng.Fetch(ctx, slow, "blob.bin", p)
	if err != nil {
		t.Fatalf("fetch with a slow upload: %v", err)
	}
	_ = readAll(t, res)
	if a, _ := testDB.GetArtifact(ctx, slow.Name, "blob.bin"); a == nil {
		t.Error("an upload outlasting the request deadline must still cache")
	}

	fs.mode.Store(storeFailPut)
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("unstored blob"))
	}))
	t.Cleanup(other.Close)
	failedRemote := genericRemote("upload-failed")
	failedRemote.BaseURL = other.URL
	failed := seed(t, failedRemote)
	res, err = eng.Fetch(ctx, failed, "blob.bin", p)
	if err != nil {
		t.Fatalf("fetch with a failing upload: %v", err)
	}
	if body := readAll(t, res); body != "unstored blob" {
		t.Errorf("body = %q, want the fetched bytes served", body)
	}
	if a, _ := testDB.GetArtifact(ctx, failed.Name, "blob.bin"); a != nil {
		t.Error("a failed upload must not be recorded")
	}

	if val, _ := storeHealthKey(t); val != "up" {
		t.Errorf("s3:health = %q after upload outcomes, want unchanged up", val)
	}
}
