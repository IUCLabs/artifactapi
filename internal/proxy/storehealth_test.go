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

	h := &hysteresis{cfg: ProbeConfig{UpThreshold: 3, DownThreshold: 3}}
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

	cfg := ProbeConfig{UpThreshold: 2, DownThreshold: 2, Interval: 200 * time.Millisecond, Timeout: 100 * time.Millisecond}
	called := make(chan struct{})
	results := make(chan error)
	probe := func(pctx context.Context) error {
		if dl, ok := pctx.Deadline(); !ok || time.Until(dl) > cfg.Timeout {
			t.Errorf("probe deadline = %v (set %v), want within %s", dl, ok, cfg.Timeout)
		}
		called <- struct{}{}
		return <-results
	}
	done := make(chan struct{})
	go func() {
		RunStoreProbe(ctx, cfg, nil, probe, testCache)
		close(done)
	}()
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
		if want != "" && (ttl <= 3*cfg.Interval || ttl > healthTTL(cfg)) {
			t.Fatalf("s3:health TTL = %s, want (%s, %s]", ttl, 3*cfg.Interval, healthTTL(cfg))
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
	select {
	case <-done:
	case <-time.After(cfg.Interval):
		t.Fatal("RunStoreProbe did not return after cancel")
	}
}

// faultyStore proxies a real MinIO, counting requests, and can hang or fail them.
type faultyStore struct {
	mode     atomic.Int32
	requests atomic.Int64
	putDelay time.Duration
	put      atomic.Bool
}

const (
	storePass int32 = iota
	storeHang
	storeFailPut
	storeSlowPut
	storeSlowHeadAfterPut
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
		case mode == storeSlowPut && r.Method == http.MethodPut,
			mode == storeSlowHeadAfterPut && r.Method == http.MethodHead && fs.put.Load():
			time.Sleep(fs.putDelay)
		}
		if r.Method == http.MethodPut {
			fs.put.Store(true)
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

	tagged := seed(t, models.Remote{Name: "keydown-npm-etag", PackageType: models.PackageNPM, RepoType: models.RepoTypeRemote, BaseURL: upstream.URL, CheckMutable: true})
	if err := testCache.SetETag(ctx, tagged.Name, "pkg", `"v1"`, time.Minute); err != nil {
		t.Fatal(err)
	}
	res, err = eng.Fetch(ctx, tagged, "pkg", prov(t, models.PackageNPM))
	if err != nil {
		t.Fatalf("index fetch with an ETag and key down: %v", err)
	}
	if body := readAll(t, res); res.Source != "remote" || !strings.Contains(body, `"name":"pkg"`) {
		t.Errorf("index with an ETag: source=%s body=%q, want served from upstream", res.Source, body)
	}
	if n := fs.requests.Load(); n != 0 {
		t.Errorf("S3 requests for an index with an ETag and key down = %d, want 0", n)
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

	locked := seed(t, genericRemote("keydown-locked"))
	token, err := testCache.AcquireLock(ctx, locked.Name, "blob.bin", time.Minute)
	if err != nil || token == "" {
		t.Fatalf("acquire lock: %q %v", token, err)
	}
	start := time.Now()
	res, err = eng.Fetch(ctx, locked, "blob.bin", prov(t, models.PackageGeneric))
	if err != nil {
		t.Fatalf("fetch with the lock held elsewhere and key down: %v", err)
	}
	if body := readAll(t, res); res.Source != "remote" || body != "immutable blob" {
		t.Errorf("locked: source=%s body=%q", res.Source, body)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("fetch with the lock held elsewhere took %s, want no store wait", elapsed)
	}

	open := seed(t, genericRemote("keydown-circuit"))
	for i := 0; i < 6; i++ {
		eng.circuit.RecordFailure(ctx, open.Name)
	}
	_, err = eng.Fetch(ctx, open, "blob.bin", prov(t, models.PackageGeneric))
	var perr *ProxyError
	if !errors.As(err, &perr) || perr.Status != http.StatusServiceUnavailable {
		t.Errorf("circuit open with key down: err = %v, want 503", err)
	}

	dead := genericRemote("keydown-stale")
	dead.BaseURL = "http://127.0.0.1:1"
	dead = seed(t, dead)
	if _, err := eng.Fetch(ctx, dead, "blob.bin", prov(t, models.PackageGeneric)); err == nil {
		t.Error("stale_on_error with key down must fail on a dead upstream")
	}
	if fresh, _ := testCache.CheckTTL(ctx, dead.Name, "blob.bin"); fresh {
		t.Error("stale_on_error with key down must not set a TTL")
	}

	q := quarantineRemote(t, "keydown-quarantine", true)
	_, err = eng.Fetch(ctx, q, "fresh.tgz", prov(t, models.PackageNPM))
	_ = wantQuarantined(t, err)

	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := eng.Fetch(cctx, blob, "blob.bin", prov(t, models.PackageGeneric)); err == nil {
		t.Error("a cancelled request must fail with the key down")
	}

	if n := fs.requests.Load(); n != 0 {
		t.Errorf("S3 requests with the key down = %d, want 0", n)
	}
	assertEmptyDir(t, tmp)

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

func TestHungStoreFreshHitFallsBack(t *testing.T) {
	requireStack(t)
	clearStoreHealth(t)
	ctx := context.Background()
	fs, s3 := newFaultyStore(t, "proxy-hung-fresh")
	eng := NewEngine(testDB, testCache, s3)
	setStoreUp(t, true)
	p := prov(t, models.PackageGeneric)

	blob := seed(t, genericRemote("hung-fresh"))
	res, err := eng.Fetch(ctx, blob, "blob.bin", p)
	if err != nil {
		t.Fatal(err)
	}
	_ = readAll(t, res)
	if fresh, _ := testCache.CheckTTL(ctx, blob.Name, "blob.bin"); !fresh {
		t.Fatal("first fetch must leave a fresh TTL")
	}

	fs.mode.Store(storeHang)
	start := time.Now()
	res, err = eng.Fetch(ctx, blob, "blob.bin", p)
	if err != nil {
		t.Fatalf("fresh hit on a hung store: %v", err)
	}
	if body := readAll(t, res); res.Source != "remote" || body != "immutable blob" {
		t.Errorf("fresh hit on a hung store: source=%s body=%q", res.Source, body)
	}
	// one head deadline in serveFromStore, one in the CAS Exists
	if elapsed := time.Since(start); elapsed > 2*storage.RequestTimeout+time.Second {
		t.Errorf("fresh hit on a hung store took %s, want about %s", elapsed, 2*storage.RequestTimeout)
	}
	if val, _ := storeHealthKey(t); val != "up" {
		t.Errorf("s3:health = %q after a request-path failure, want unchanged up", val)
	}
}

func TestHungStoreHeadFallsBack(t *testing.T) {
	requireStack(t)
	clearStoreHealth(t)
	ctx := context.Background()
	fs, s3 := newFaultyStore(t, "proxy-hung-head")
	eng := NewEngine(testDB, testCache, s3)
	setStoreUp(t, true)
	fs.mode.Store(storeHang)

	blob := seed(t, genericRemote("hung-head"))
	start := time.Now()
	h, err := eng.Head(ctx, blob, "blob.bin", prov(t, models.PackageGeneric))
	if err != nil {
		t.Fatalf("head on a hung store: %v", err)
	}
	if h.Source != "remote" {
		t.Errorf("head on a hung store: source=%s, want remote", h.Source)
	}
	if elapsed := time.Since(start); elapsed > storage.RequestTimeout+time.Second {
		t.Errorf("head on a hung store took %s, want about %s", elapsed, storage.RequestTimeout)
	}
	if fs.requests.Load() == 0 {
		t.Error("head with key up must try the store")
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

	fs.mode.Store(storeSlowHeadAfterPut)
	fs.put.Store(false)
	fresh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("slow head blob"))
	}))
	t.Cleanup(fresh.Close)
	slowHeadRemote := genericRemote("upload-slow-head")
	slowHeadRemote.BaseURL = fresh.URL
	slowHead := seed(t, slowHeadRemote)
	res, err = eng.Fetch(ctx, slowHead, "blob.bin", p)
	if err != nil {
		t.Fatalf("fetch with a slow head after upload: %v", err)
	}
	if body := readAll(t, res); body != "slow head blob" {
		t.Errorf("body = %q", body)
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

func intp(v int) *int { return &v }

func TestProbeConfigApply(t *testing.T) {
	def := ProbeConfig{UpThreshold: 3, DownThreshold: 3, Interval: 3 * time.Second, Timeout: 1500 * time.Millisecond}
	if got := def.Apply(models.Settings{}); got != def {
		t.Fatalf("empty settings changed config: %+v", got)
	}
	got := def.Apply(models.Settings{S3HealthDownThreshold: intp(7), S3HealthIntervalMs: intp(5000)})
	want := ProbeConfig{UpThreshold: 3, DownThreshold: 7, Interval: 5 * time.Second, Timeout: 1500 * time.Millisecond}
	if got != want {
		t.Fatalf("apply = %+v, want %+v", got, want)
	}
	if back := def.Apply(got.Settings()); back != got {
		t.Fatalf("Settings round trip = %+v, want %+v", back, got)
	}
	for name, c := range map[string]ProbeConfig{
		"zero up":            {UpThreshold: 0, DownThreshold: 1, Interval: time.Second, Timeout: 100 * time.Millisecond},
		"zero down":          {UpThreshold: 1, DownThreshold: 0, Interval: time.Second, Timeout: 100 * time.Millisecond},
		"timeout = interval": {UpThreshold: 1, DownThreshold: 1, Interval: time.Second, Timeout: time.Second},
		"interval too short": {UpThreshold: 1, DownThreshold: 1, Interval: 499 * time.Millisecond, Timeout: 100 * time.Millisecond},
		"timeout too short":  {UpThreshold: 1, DownThreshold: 1, Interval: time.Second, Timeout: 99 * time.Millisecond},
		"threshold too high": {UpThreshold: 101, DownThreshold: 1, Interval: time.Second, Timeout: 100 * time.Millisecond},
	} {
		if c.Validate() == nil {
			t.Errorf("%s: validated", name)
		}
	}
}

func TestProbeSettingsRefresh(t *testing.T) {
	def := ProbeConfig{UpThreshold: 3, DownThreshold: 3, Interval: 3 * time.Second, Timeout: 1500 * time.Millisecond}
	var stored models.Settings
	var loadErr error
	p := probeSettings{defaults: def, load: func(context.Context) (models.Settings, error) { return stored, loadErr }}

	stored = models.Settings{S3HealthUpThreshold: intp(1), S3HealthIntervalMs: intp(2000)}
	cfg := p.refresh(context.Background(), def)
	if cfg.UpThreshold != 1 || cfg.Interval != 2*time.Second || cfg.DownThreshold != 3 {
		t.Fatalf("refresh = %+v", cfg)
	}

	loadErr = errors.New("db down")
	if got := p.refresh(context.Background(), cfg); got != cfg {
		t.Fatalf("a load error must keep the last known config, got %+v", got)
	}

	loadErr = nil
	stored = models.Settings{S3HealthIntervalMs: intp(1000)}
	if got := p.refresh(context.Background(), cfg); got != cfg {
		t.Fatalf("an invalid effective config (timeout >= interval) must be ignored, got %+v", got)
	}

	stored = models.Settings{}
	if got := p.refresh(context.Background(), cfg); got != def {
		t.Fatalf("cleared settings must revert to defaults, got %+v", got)
	}

	static := probeSettings{defaults: def}
	if got := static.refresh(context.Background(), cfg); got != cfg {
		t.Fatalf("nil loader changed config: %+v", got)
	}
}

func TestProbeSettingsLoadTimeoutIgnoresProbeTimeout(t *testing.T) {
	def := ProbeConfig{UpThreshold: 3, DownThreshold: 3, Interval: 3 * time.Second, Timeout: 1500 * time.Millisecond}
	var budget time.Duration
	p := probeSettings{defaults: def, load: func(ctx context.Context) (models.Settings, error) {
		deadline, _ := ctx.Deadline()
		budget = time.Until(deadline)
		return models.Settings{S3HealthTimeoutMs: intp(100), S3HealthIntervalMs: intp(500)}, nil
	}}
	cfg := p.refresh(context.Background(), def)
	if cfg.Timeout != 100*time.Millisecond {
		t.Fatalf("refresh = %+v", cfg)
	}
	p.refresh(context.Background(), cfg)
	if budget < settingsLoadTimeout-time.Second || budget > settingsLoadTimeout {
		t.Fatalf("settings load budget = %s under a 100ms probe timeout, want about %s", budget, settingsLoadTimeout)
	}
}

func TestRunStoreProbePicksUpSettings(t *testing.T) {
	requireStack(t)
	clearStoreHealth(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	def := ProbeConfig{UpThreshold: 1, DownThreshold: 3, Interval: 500 * time.Millisecond, Timeout: 100 * time.Millisecond}
	type loaded struct {
		s   models.Settings
		err error
	}
	var current atomic.Pointer[loaded]
	current.Store(&loaded{})
	load := func(context.Context) (models.Settings, error) {
		l := current.Load()
		return l.s, l.err
	}
	called := make(chan struct{})
	results := make(chan error)
	probe := func(context.Context) error {
		called <- struct{}{}
		return <-results
	}
	done := make(chan struct{})
	go func() {
		RunStoreProbe(ctx, def, load, probe, testCache)
		close(done)
	}()
	<-called

	fail := errors.New("probe failed")
	step := func(err error, want string) {
		t.Helper()
		results <- err
		<-called
		if val, _ := storeHealthKey(t); val != want {
			t.Fatalf("s3:health = %q, want %q", val, want)
		}
	}
	// Holding the in-flight probe past a settings refresh queues the change for the next probe.
	settle := func(l *loaded) {
		current.Store(l)
		time.Sleep(settingsRefreshInterval + 300*time.Millisecond)
	}
	step(nil, "up")
	settle(&loaded{s: models.Settings{S3HealthUpThreshold: intp(2), S3HealthDownThreshold: intp(1)}})
	step(fail, "up")
	step(fail, "down")

	// A read failure keeps up_threshold 2 rather than reverting to the default of 1.
	settle(&loaded{err: errors.New("db down")})
	step(nil, "down")
	step(nil, "up")

	cancel()
	results <- nil
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("RunStoreProbe did not return after cancel")
	}
}

func TestRunStoreProbeFirstWriteWaitsOutStalledSettings(t *testing.T) {
	requireStack(t)
	clearStoreHealth(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	def := ProbeConfig{UpThreshold: 1, DownThreshold: 1, Interval: 500 * time.Millisecond, Timeout: 100 * time.Millisecond}
	load := func(lctx context.Context) (models.Settings, error) {
		<-lctx.Done()
		return models.Settings{}, lctx.Err()
	}
	probe := func(context.Context) error { return errors.New("probe failed") }
	done := make(chan struct{})
	go func() {
		RunStoreProbe(ctx, def, load, probe, testCache)
		close(done)
	}()

	deadline := time.Now().Add(settingsLoadTimeout + time.Second)
	for val, _ := storeHealthKey(t); val != "down"; val, _ = storeHealthKey(t) {
		if time.Now().After(deadline) {
			t.Fatal("no store health written within the stalled settings budget")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if _, ttl := storeHealthKey(t); ttl < healthTTL(def)-time.Second {
		t.Fatalf("s3:health TTL = %s after a stalled first settings read, want about %s", ttl, healthTTL(def))
	}

	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("RunStoreProbe did not return after cancel")
	}
}

func TestRunStoreProbeRewritesTTLOnIntervalChange(t *testing.T) {
	requireStack(t)
	clearStoreHealth(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	def := ProbeConfig{UpThreshold: 1, DownThreshold: 1, Interval: time.Hour, Timeout: 100 * time.Millisecond}
	var interval atomic.Int64
	interval.Store(500)
	load := func(context.Context) (models.Settings, error) {
		return models.Settings{S3HealthIntervalMs: intp(int(interval.Load()))}, nil
	}
	var probes atomic.Int64
	probe := func(context.Context) error {
		probes.Add(1)
		return nil
	}
	done := make(chan struct{})
	go func() {
		RunStoreProbe(ctx, def, load, probe, testCache)
		close(done)
	}()

	short := def
	short.Interval = 500 * time.Millisecond
	long := def
	long.Interval = 10 * time.Second
	waitKey := func(within time.Duration, ok func(string, time.Duration) bool) {
		t.Helper()
		deadline := time.Now().Add(within)
		for {
			val, ttl := storeHealthKey(t)
			if ok(val, ttl) {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("s3:health = %q with TTL %s after %s", val, ttl, within)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	waitKey(2*time.Second, func(val string, ttl time.Duration) bool {
		return val == "up" && ttl > 0 && ttl <= healthTTL(short)
	})

	interval.Store(long.Interval.Milliseconds())
	waitKey(settingsRefreshInterval+500*time.Millisecond, func(val string, ttl time.Duration) bool {
		return val == "up" && ttl > healthTTL(long)-time.Second
	})
	seen := probes.Load()
	time.Sleep(healthTTL(short) + 500*time.Millisecond)
	if val, _ := storeHealthKey(t); val != "up" {
		t.Fatalf("s3:health = %q after the old TTL lapsed, want up", val)
	}
	if n := probes.Load() - seen; n != 0 {
		t.Fatalf("%d probes ran during the new 10s interval, want none so only the rewrite kept the key", n)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("RunStoreProbe did not return after cancel")
	}
}

func TestRunStoreProbeFollowsIntervalChange(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Thresholds are never crossed, so the probe never writes to Redis.
	def := ProbeConfig{UpThreshold: 100, DownThreshold: 100, Interval: time.Hour, Timeout: time.Second}
	var interval atomic.Int64
	interval.Store(500)
	load := func(context.Context) (models.Settings, error) {
		return models.Settings{S3HealthIntervalMs: intp(int(interval.Load())), S3HealthTimeoutMs: intp(100)}, nil
	}
	ticks := make(chan time.Time, 1000)
	probe := func(context.Context) error {
		ticks <- time.Now()
		return errors.New("probe failed")
	}
	done := make(chan struct{})
	go func() {
		RunStoreProbe(ctx, def, load, probe, nil)
		close(done)
	}()
	next := func() time.Time {
		t.Helper()
		select {
		case at := <-ticks:
			return at
		case <-time.After(5 * time.Second):
			t.Fatal("no probe within 5s")
			return time.Time{}
		}
	}

	// The stored 500ms interval replaces the hour-long default before the first tick.
	for range 2 {
		next()
	}

	interval.Store(1500)
	next()
	prev := next()
	if gap := next().Sub(prev); gap < 1200*time.Millisecond {
		t.Fatalf("probe gap after raising the interval to 1.5s = %s", gap)
	}

	interval.Store(500)
	next()
	prev = next()
	if gap := next().Sub(prev); gap > 1000*time.Millisecond {
		t.Fatalf("probe gap after lowering the interval to 500ms = %s", gap)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("RunStoreProbe did not return after cancel")
	}
}

func TestRunStoreProbeShortensIntervalWithoutWaiting(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	def := ProbeConfig{UpThreshold: 100, DownThreshold: 100, Interval: 3 * time.Second, Timeout: 100 * time.Millisecond}
	var interval atomic.Int64
	interval.Store(60000)
	load := func(context.Context) (models.Settings, error) {
		return models.Settings{S3HealthIntervalMs: intp(int(interval.Load()))}, nil
	}
	ticks := make(chan time.Time, 100)
	probe := func(context.Context) error {
		ticks <- time.Now()
		return errors.New("probe failed")
	}
	done := make(chan struct{})
	go func() {
		RunStoreProbe(ctx, def, load, probe, nil)
		close(done)
	}()
	next := func(within time.Duration) time.Time {
		t.Helper()
		select {
		case at := <-ticks:
			return at
		case <-time.After(within):
			t.Fatalf("no probe within %s", within)
			return time.Time{}
		}
	}

	first := next(time.Second)
	interval.Store(3000)
	if gap := next(4 * time.Second).Sub(first); gap < 2500*time.Millisecond || gap > 3500*time.Millisecond {
		t.Fatalf("probe gap after shortening 60s to 3s = %s, want about 3s", gap)
	}

	interval.Store(60000)
	time.Sleep(settingsRefreshInterval + 500*time.Millisecond)
	changed := time.Now()
	interval.Store(1000)
	if lag := next(3 * time.Second).Sub(changed); lag > settingsRefreshInterval+500*time.Millisecond {
		t.Fatalf("probe %s after shortening below the time since the last probe, want within one settings refresh", lag)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("RunStoreProbe did not return after cancel")
	}
}
