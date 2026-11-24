package proxy

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/IUCLabs/artifactapi/internal/cache"
	"github.com/IUCLabs/artifactapi/internal/database"
	"github.com/IUCLabs/artifactapi/internal/provider"
	_ "github.com/IUCLabs/artifactapi/internal/provider/generic"
	_ "github.com/IUCLabs/artifactapi/internal/provider/npm"
	"github.com/IUCLabs/artifactapi/internal/storage"
	"github.com/IUCLabs/artifactapi/internal/testsupport"
	"github.com/IUCLabs/artifactapi/pkg/models"
	goredis "github.com/redis/go-redis/v9"
)

var (
	testEngine *Engine
	testCache  *cache.Redis
	testDB     *database.DB
	testRedis  string
	upstream   *httptest.Server
)

func TestMain(m *testing.M) {
	ctx := context.Background()
	dsn, termPG, err := testsupport.StartPostgres(ctx)
	if err != nil {
		os.Exit(m.Run())
	}
	redisURL, termRedis, err := testsupport.StartRedis(ctx)
	if err != nil {
		termPG()
		os.Exit(m.Run())
	}
	minio, termMinio, err := testsupport.StartMinio(ctx)
	if err != nil {
		termPG()
		termRedis()
		os.Exit(m.Run())
	}

	db, err := database.New(dsn)
	if err != nil {
		panic(err)
	}
	redis, err := cache.NewRedis(redisURL)
	if err != nil {
		panic(err)
	}
	var s3 *storage.S3
	for i := 0; i < 20; i++ {
		if s3, err = storage.NewS3(minio.Endpoint, minio.AccessKey, minio.SecretKey, "proxy-test", false, ""); err == nil {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	if err != nil {
		panic(err)
	}

	testCache = redis
	testRedis = redisURL
	testDB = db
	testEngine = NewEngine(db, redis, s3)
	upstream = httptest.NewServer(http.HandlerFunc(mockUpstream))

	code := m.Run()

	upstream.Close()
	db.Close()
	termMinio()
	termRedis()
	termPG()
	if code != 0 {
		os.Exit(code)
	}
}

func mockUpstream(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/blob.bin":
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write([]byte("immutable blob"))
	case "/pkg": // npm metadata: mutable, supports revalidation
		if r.Method == http.MethodHead && r.Header.Get("If-None-Match") == `"v1"` {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", `"v1"`)
		_, _ = w.Write([]byte(`{"name":"pkg"}`))
	case "/protected.bin": // requires a bearer token obtained from /token
		if r.Header.Get("Authorization") != "Bearer minted-token" {
			w.Header().Set("Www-Authenticate", `Bearer realm="`+upstream.URL+`/token",service="reg",scope="repo:pull"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte("protected payload"))
	case "/protected2.bin": // same challenge as /protected.bin
		if r.Header.Get("Authorization") != "Bearer minted-token" {
			w.Header().Set("Www-Authenticate", `Bearer realm="`+upstream.URL+`/token",service="reg",scope="repo:pull"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte("protected payload 2"))
	case "/token":
		_, _ = w.Write([]byte(`{"token":"minted-token","expires_in":300}`))
	case "/token-at":
		_, _ = w.Write([]byte(`{"access_token":"at-token"}`))
	case "/token-500":
		w.WriteHeader(http.StatusInternalServerError)
	case "/err500":
		w.WriteHeader(http.StatusInternalServerError)
	case "/noauth": // 401 with an unusable challenge (no realm)
		w.Header().Set("Www-Authenticate", `Bearer service="reg"`)
		w.WriteHeader(http.StatusUnauthorized)
	default:
		http.NotFound(w, r)
	}
}

func requireStack(t *testing.T) {
	t.Helper()
	if testEngine == nil {
		t.Skip("Docker unavailable; skipping proxy engine test")
	}
}

func genericRemote(name string) models.Remote {
	return models.Remote{Name: name, PackageType: models.PackageGeneric, RepoType: models.RepoTypeRemote, BaseURL: upstream.URL, StaleOnError: true}
}

// seed inserts the remote so artifact rows (FK to remotes) can be stored.
func seed(t *testing.T, r models.Remote) models.Remote {
	t.Helper()
	rr := r
	if err := testDB.CreateRemote(context.Background(), &rr); err != nil {
		t.Fatalf("seed remote %s: %v", r.Name, err)
	}
	return r
}

func prov(t *testing.T, pt models.PackageType) provider.Provider {
	p, err := provider.Get(pt)
	if err != nil {
		t.Fatalf("provider %s: %v", pt, err)
	}
	return p
}

func readAll(t *testing.T, res *FetchResult) string {
	t.Helper()
	defer func() { _ = res.Reader.Close() }()
	b, _ := io.ReadAll(res.Reader)
	return string(b)
}

func TestFetchImmutableMissThenHit(t *testing.T) {
	requireStack(t)
	ctx := context.Background()
	r := seed(t, genericRemote("eng-imm"))
	p := prov(t, models.PackageGeneric)

	res, err := testEngine.Fetch(ctx, r, "blob.bin", p)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if res.Source != "remote" || readAll(t, res) != "immutable blob" {
		t.Errorf("miss: source=%s", res.Source)
	}
	res, err = testEngine.Fetch(ctx, r, "blob.bin", p)
	if err != nil {
		t.Fatal(err)
	}
	if res.Source != "cache" || readAll(t, res) != "immutable blob" {
		t.Errorf("hit: source=%s", res.Source)
	}
}

func TestFetchDenied(t *testing.T) {
	requireStack(t)
	r := genericRemote("eng-deny")
	r.Blocklist = []string{`\.secret$`}
	_, err := testEngine.Fetch(context.Background(), r, "x.secret", prov(t, models.PackageGeneric))
	var pe *ProxyError
	if err == nil || !asProxyError(err, &pe) || pe.Status != http.StatusForbidden {
		t.Errorf("expected 403 ProxyError, got %v", err)
	}
}

func TestHead(t *testing.T) {
	requireStack(t)
	ctx := context.Background()
	r := seed(t, genericRemote("eng-head"))
	p := prov(t, models.PackageGeneric)

	// Uncached HEAD hits upstream.
	h, err := testEngine.Head(ctx, r, "blob.bin", p)
	if err != nil || h.Source != "remote" {
		t.Fatalf("head uncached: %+v %v", h, err)
	}
	// Populate the cache, then HEAD should be served from metadata.
	res, _ := testEngine.Fetch(ctx, r, "blob.bin", p)
	_ = res.Reader.Close()
	h, err = testEngine.Head(ctx, r, "blob.bin", p)
	if err != nil || h.Source != "cache" {
		t.Errorf("head cached: %+v %v", h, err)
	}
	// Denied HEAD.
	r.Blocklist = []string{".*"}
	if _, err := testEngine.Head(ctx, r, "blob.bin", p); err == nil {
		t.Error("expected denied head error")
	}
}

func TestHeadUpstreamUsesRemoteTimeout(t *testing.T) {
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(3 * time.Second)
	}))
	defer slow.Close()
	r := models.Remote{Name: "slow-head", PackageType: models.PackageGeneric, BaseURL: slow.URL, UpstreamResponseHeaderTimeout: 1}

	start := time.Now()
	_, err := (&Engine{}).headUpstreamOnce(context.Background(), r, "blob.bin", prov(t, models.PackageGeneric), ClassImmutable)
	if err == nil || time.Since(start) > 2*time.Second {
		t.Fatalf("HEAD ignored the remote's response header timeout: err=%v after %s", err, time.Since(start))
	}
}

func TestStaleOnError(t *testing.T) {
	requireStack(t)
	ctx := context.Background()
	r := seed(t, genericRemote("eng-stale"))
	p := prov(t, models.PackageGeneric)

	if _, err := testEngine.Fetch(ctx, r, "blob.bin", p); err != nil {
		t.Fatal(err)
	}
	// Drop cache freshness so the next fetch goes upstream, then point at a
	// dead upstream: stale-on-error must serve the stored copy.
	if err := testCache.FlushRemote(ctx, "eng-stale"); err != nil {
		t.Fatal(err)
	}
	r.BaseURL = "http://127.0.0.1:1"
	res, err := testEngine.Fetch(ctx, r, "blob.bin", p)
	if err != nil {
		t.Fatalf("expected stale serve, got %v", err)
	}
	if res.Source != "cache" || readAll(t, res) != "immutable blob" {
		t.Errorf("stale: source=%s", res.Source)
	}
}

func TestCircuitOpenServesStale(t *testing.T) {
	requireStack(t)
	ctx := context.Background()
	r := seed(t, genericRemote("eng-circuit"))
	p := prov(t, models.PackageGeneric)
	if _, err := testEngine.Fetch(ctx, r, "blob.bin", p); err != nil {
		t.Fatal(err)
	}
	if err := testCache.FlushRemote(ctx, "eng-circuit"); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 6; i++ {
		testEngine.circuit.RecordFailure(ctx, "eng-circuit")
	}
	res, err := testEngine.Fetch(ctx, r, "blob.bin", p)
	if err != nil {
		t.Fatalf("circuit-open should serve stale: %v", err)
	}
	if res.Source != "cache" {
		t.Errorf("expected stale from open circuit, got %s", res.Source)
	}
	_ = res.Reader.Close()
}

// expire drops path's freshness key so the next fetch revalidates.
func expire(t *testing.T, remote, path string) {
	t.Helper()
	if err := testCache.SetTTL(context.Background(), remote, path, time.Millisecond); err != nil {
		t.Fatal(err)
	}
	time.Sleep(5 * time.Millisecond)
}

func mutableFetch(t *testing.T, r models.Remote, path string) (string, string) {
	t.Helper()
	res, err := testEngine.Fetch(context.Background(), r, path, prov(t, models.PackageRPM))
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	return readAll(t, res), res.Source
}

const repomd = "8/Everything/x86_64/repodata/repomd.xml"

func TestMutableRevalidation(t *testing.T) {
	requireStack(t)
	srv, rev, conditional := changingUpstream(t)
	r := seed(t, models.Remote{Name: "eng-reval", PackageType: models.PackageRPM, RepoType: models.RepoTypeRemote, BaseURL: srv.URL, CheckMutable: true, MutableTTL: 60})

	if body, _ := mutableFetch(t, r, repomd); body != "rev1:/"+repomd {
		t.Fatalf("initial fetch = %q", body)
	}
	expire(t, r.Name, repomd)
	if body, src := mutableFetch(t, r, repomd); body != "rev1:/"+repomd || src != "cache" {
		t.Errorf("after expiry = %q from %s, want cached rev1", body, src)
	}
	if n := conditional.Load(); n != 1 {
		t.Errorf("conditional requests = %d, want 1 revalidation", n)
	}
	if fresh, _ := testCache.CheckTTL(context.Background(), r.Name, repomd); !fresh {
		t.Error("304 should re-arm the freshness key")
	}

	rev.Store("rev2")
	expire(t, r.Name, repomd)
	if body, src := mutableFetch(t, r, repomd); body != "rev2:/"+repomd || src != "remote" {
		t.Errorf("after upstream change = %q from %s, want remote rev2", body, src)
	}
}

func etagKeyTTL(t *testing.T, remote, path string) time.Duration {
	t.Helper()
	opts, err := goredis.ParseURL(testRedis)
	if err != nil {
		t.Fatal(err)
	}
	c := goredis.NewClient(opts)
	defer func() { _ = c.Close() }()
	d, err := c.PTTL(context.Background(), "etag:"+remote+":"+path).Result()
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// The ETag is only kept when it can be used, and never without an expiry.
func TestMutableETagStorage(t *testing.T) {
	requireStack(t)
	srv, _, _ := changingUpstream(t)
	for _, tc := range []struct {
		name  string
		check bool
		ttl   int
		want  time.Duration
	}{
		{"eng-etag-off", false, 60, -2},
		{"eng-etag-never", true, 0, -2},
		{"eng-etag-short", true, 60, 7 * 24 * time.Hour},
		{"eng-etag-long", true, 86400, 10 * 24 * time.Hour},
	} {
		r := seed(t, models.Remote{Name: tc.name, PackageType: models.PackageRPM, RepoType: models.RepoTypeRemote, BaseURL: srv.URL, CheckMutable: tc.check, MutableTTL: tc.ttl})
		mutableFetch(t, r, repomd)
		got := etagKeyTTL(t, tc.name, repomd)
		if tc.want < 0 {
			if got != tc.want {
				t.Errorf("%s: etag key ttl = %v, want no key", tc.name, got)
			}
			continue
		}
		if got <= tc.want-time.Minute || got > tc.want {
			t.Errorf("%s: etag key ttl = %v, want ~%v", tc.name, got, tc.want)
		}
	}
}

// Immutable paths are never revalidated, so their upstream ETag is not kept.
func TestImmutableFetchStoresNoETag(t *testing.T) {
	requireStack(t)
	srv, _, _ := changingUpstream(t)
	r := seed(t, models.Remote{Name: "eng-etag-imm", PackageType: models.PackageRPM, RepoType: models.RepoTypeRemote, BaseURL: srv.URL, CheckMutable: true, MutableTTL: 60})
	const rpm = "Packages/e2e-1.0-1.noarch.rpm"
	if body, src := mutableFetch(t, r, rpm); body != "rev1:/"+rpm || src != "remote" {
		t.Fatalf("fetch = %q from %s", body, src)
	}
	if got := etagKeyTTL(t, r.Name, rpm); got != -2 {
		t.Errorf("etag key ttl = %v, want no key", got)
	}
}

// A 304 for a path whose S3 body is gone falls back to a full refetch.
func TestNotModifiedWithEvictedBodyRefetches(t *testing.T) {
	requireStack(t)
	srv, _, conditional := changingUpstream(t)
	r := seed(t, models.Remote{Name: "eng-304-gone", PackageType: models.PackageRPM, RepoType: models.RepoTypeRemote, BaseURL: srv.URL, CheckMutable: true, MutableTTL: 60})

	mutableFetch(t, r, repomd)
	if err := testEngine.store.Delete(context.Background(), storage.IndexKey(r.Name, repomd)); err != nil {
		t.Fatal(err)
	}
	expire(t, r.Name, repomd)
	if body, src := mutableFetch(t, r, repomd); body != "rev1:/"+repomd || src != "remote" {
		t.Errorf("got %q from %s, want refetched rev1 from remote", body, src)
	}
	if n := conditional.Load(); n != 1 {
		t.Errorf("conditional requests = %d, want the 304 HEAD first", n)
	}
}

// A failing revalidation HEAD still serves the stored copy with stale_on_error.
func TestStaleOnErrorWhenRevalidationFails(t *testing.T) {
	requireStack(t)
	srv, _, _ := changingUpstream(t)
	r := seed(t, models.Remote{Name: "eng-stale-head", PackageType: models.PackageRPM, RepoType: models.RepoTypeRemote, BaseURL: srv.URL, CheckMutable: true, MutableTTL: 60, StaleOnError: true})

	mutableFetch(t, r, repomd)
	srv.Close()
	expire(t, r.Name, repomd)
	if body, src := mutableFetch(t, r, repomd); body != "rev1:/"+repomd || src != "cache" {
		t.Errorf("got %q from %s, want stale rev1 from cache", body, src)
	}
}

// After a remote is deleted and flushed, a re-created remote of the same name
// fetches from its own upstream instead of revalidating the old ETag.
func TestRecreatedRemoteHasNoStaleState(t *testing.T) {
	requireStack(t)
	ctx := context.Background()
	oldSrv, _, _ := changingUpstream(t)
	r := seed(t, models.Remote{Name: "eng-recreate", PackageType: models.PackageRPM, RepoType: models.RepoTypeRemote, BaseURL: oldSrv.URL, CheckMutable: true, MutableTTL: 60})
	mutableFetch(t, r, repomd)

	if err := testDB.DeleteRemote(ctx, r.Name); err != nil {
		t.Fatal(err)
	}
	if err := testCache.FlushRemote(ctx, r.Name); err != nil {
		t.Fatal(err)
	}
	newSrv, rev, conditional := changingUpstream(t)
	rev.Store("rev9")
	r.BaseURL = newSrv.URL
	seed(t, r)
	if body, src := mutableFetch(t, r, repomd); body != "rev9:/"+repomd || src != "remote" {
		t.Errorf("got %q from %s, want rev9 from the new upstream", body, src)
	}
	if n := conditional.Load(); n != 0 {
		t.Errorf("conditional requests = %d, want none after flush", n)
	}
}

func TestBearerTokenFlow(t *testing.T) {
	requireStack(t)
	ctx := context.Background()
	r := seed(t, genericRemote("eng-bearer"))
	p := prov(t, models.PackageGeneric)

	// GET: 401 challenge -> token endpoint -> retry with bearer -> 200.
	res, err := testEngine.Fetch(ctx, r, "protected.bin", p)
	if err != nil {
		t.Fatalf("bearer fetch: %v", err)
	}
	if readAll(t, res) != "protected payload" {
		t.Error("bearer-protected content mismatch")
	}

	// A second protected path with the same challenge reuses the cached token.
	res2, err := testEngine.Fetch(ctx, r, "protected2.bin", p)
	if err != nil {
		t.Fatalf("second bearer fetch: %v", err)
	}
	if readAll(t, res2) != "protected payload 2" {
		t.Error("second bearer content mismatch")
	}

	// HEAD path also negotiates a bearer token (uncached).
	if err := testCache.FlushRemote(ctx, "eng-bearer"); err != nil {
		t.Fatal(err)
	}
	_ = testDB.DeleteArtifact(ctx, "eng-bearer", "protected.bin")
	if h, err := testEngine.Head(ctx, r, "protected.bin", p); err != nil || h.Source != "cache" && h.Source != "remote" {
		t.Fatalf("bearer head: %+v %v", h, err)
	}
}

func TestFetchUpstreamError(t *testing.T) {
	requireStack(t)
	r := seed(t, genericRemote("eng-404"))
	// Upstream 404 (no cached copy, stale-on-error can't help) -> ProxyError.
	_, err := testEngine.Fetch(context.Background(), r, "missing", prov(t, models.PackageGeneric))
	var pe *ProxyError
	if err == nil || !asProxyError(err, &pe) || pe.Status != http.StatusNotFound {
		t.Errorf("expected 404 ProxyError, got %v", err)
	}
	// HEAD of a missing upstream path also errors.
	if _, err := testEngine.Head(context.Background(), r, "missing", prov(t, models.PackageGeneric)); err == nil {
		t.Error("expected head error for missing path")
	}
}

func TestFetchUpstreamStatusErrors(t *testing.T) {
	requireStack(t)
	ctx := context.Background()
	p := prov(t, models.PackageGeneric)

	r := seed(t, genericRemote("eng-500"))
	_, err := testEngine.Fetch(ctx, r, "err500", p)
	var pe *ProxyError
	if err == nil || !asProxyError(err, &pe) || pe.Status != http.StatusInternalServerError {
		t.Errorf("expected 500 ProxyError, got %v", err)
	}

	r = seed(t, genericRemote("eng-noauth"))
	_, err = testEngine.Fetch(ctx, r, "noauth", p)
	if err == nil || !asProxyError(err, &pe) || pe.Status != http.StatusUnauthorized {
		t.Errorf("expected 401 ProxyError, got %v", err)
	}
}

func TestBearerTokenParsing(t *testing.T) {
	// Non-Bearer challenges and missing realms are rejected.
	if _, _, err := fetchBearerToken(context.Background(), "Basic realm=x", models.Remote{}); err == nil {
		t.Error("expected error for non-Bearer challenge")
	}
	if _, _, err := fetchBearerToken(context.Background(), `Bearer service="reg"`, models.Remote{}); err == nil {
		t.Error("expected error for missing realm")
	}
}

func TestWaitForStoreCoalesces(t *testing.T) {
	requireStack(t)
	ctx := context.Background()
	r := seed(t, genericRemote("eng-herd"))
	p := prov(t, models.PackageGeneric)

	// Fire concurrent cold-cache fetches: only one holds the lock, the others
	// wait on the store (waitForStore) and pick up the result.
	const n = 4
	done := make(chan string, n)
	for i := 0; i < n; i++ {
		go func() {
			res, err := testEngine.Fetch(ctx, r, "blob.bin", p)
			if err != nil {
				done <- "err:" + err.Error()
				return
			}
			done <- readAll(t, res)
		}()
	}
	for i := 0; i < n; i++ {
		if got := <-done; got != "immutable blob" {
			t.Errorf("concurrent fetch got %q", got)
		}
	}
}

func TestRevalidationUpstreamError(t *testing.T) {
	requireStack(t)
	ctx := context.Background()
	r := seed(t, models.Remote{Name: "eng-reval-err", PackageType: models.PackageNPM, RepoType: models.RepoTypeRemote, BaseURL: upstream.URL, CheckMutable: true, MutableTTL: 3600, StaleOnError: true})
	p := prov(t, models.PackageNPM)

	res, err := testEngine.Fetch(ctx, r, "pkg", p)
	if err != nil {
		t.Fatalf("initial fetch: %v", err)
	}
	_ = res.Reader.Close()

	// Expire freshness but keep the ETag, then break the upstream: the
	// conditional HEAD (checkUpstream) errors, and stale-on-error serves the
	// stored index.
	if err := testCache.SetTTL(ctx, "eng-reval-err", "pkg", time.Millisecond); err != nil {
		t.Fatal(err)
	}
	time.Sleep(10 * time.Millisecond)
	r.BaseURL = "http://127.0.0.1:1"
	res, err = testEngine.Fetch(ctx, r, "pkg", p)
	if err != nil {
		t.Fatalf("expected stale serve on revalidation error, got %v", err)
	}
	if res.Source != "cache" {
		t.Errorf("expected stale cache source, got %s", res.Source)
	}
	_ = res.Reader.Close()
}

func TestTTLFor(t *testing.T) {
	e := &Engine{}
	if got := e.ttlFor(models.Remote{ImmutableTTL: 100}, ClassImmutable); got != 100*time.Second {
		t.Errorf("immutable ttl = %v", got)
	}
	if got := e.ttlFor(models.Remote{ImmutableTTL: 0}, ClassImmutable); got != 0 {
		t.Errorf("immutable ttl=0 (forever) = %v", got)
	}
	if got := e.ttlFor(models.Remote{MutableTTL: 50}, ClassMutable); got != 50*time.Second {
		t.Errorf("mutable ttl = %v", got)
	}
}

func TestHeadUpstreamStatusError(t *testing.T) {
	requireStack(t)
	r := seed(t, genericRemote("eng-head500"))
	if _, err := testEngine.Head(context.Background(), r, "err500", prov(t, models.PackageGeneric)); err == nil {
		t.Error("expected error for HEAD of 500 upstream")
	}
}

func TestHeadCachedIndex(t *testing.T) {
	requireStack(t)
	ctx := context.Background()
	r := seed(t, models.Remote{Name: "eng-headidx", PackageType: models.PackageNPM, RepoType: models.RepoTypeRemote, BaseURL: upstream.URL, CheckMutable: true, MutableTTL: 3600})
	p := prov(t, models.PackageNPM)
	// Cache the mutable index, then HEAD is answered from the stored index.
	res, err := testEngine.Fetch(ctx, r, "pkg", p)
	if err != nil {
		t.Fatal(err)
	}
	_ = res.Reader.Close()
	h, err := testEngine.Head(ctx, r, "pkg", p)
	if err != nil || h.Source != "cache" {
		t.Errorf("head of cached index: %+v %v", h, err)
	}
}

func TestFetchBearerTokenVariants(t *testing.T) {
	requireStack(t)
	ctx := context.Background()

	// access_token field + service/scope params + basic auth on the token req.
	tok, _, err := fetchBearerToken(ctx, `Bearer realm="`+upstream.URL+`/token-at",service="reg",scope="repo:pull"`, models.Remote{Username: "u", Password: "p"})
	if err != nil || tok != "at-token" {
		t.Errorf("access_token variant: tok=%q err=%v", tok, err)
	}
	// Token endpoint error status.
	if _, _, err := fetchBearerToken(ctx, `Bearer realm="`+upstream.URL+`/token-500"`, models.Remote{}); err == nil {
		t.Error("expected error for 500 token endpoint")
	}
}

func TestCheckUpstreamChanged(t *testing.T) {
	requireStack(t)
	ctx := context.Background()
	r := genericRemote("eng-check")
	// A non-matching ETag yields a normal 200 (not 304): not modified is false.
	notModified, err := testEngine.checkUpstream(ctx, r, "pkg", `"stale-etag"`, prov(t, models.PackageNPM))
	if err != nil {
		t.Fatalf("checkUpstream: %v", err)
	}
	if notModified {
		t.Error("mismatched etag should report modified (notModified=false)")
	}
}

func TestUpstreamErrorUnwrap(t *testing.T) {
	base := context.DeadlineExceeded
	ue := &UpstreamError{Err: base}
	if ue.Unwrap() != base {
		t.Error("Unwrap should return the wrapped error")
	}
	if !isNetworkError(ue) {
		t.Error("UpstreamError should be a network error")
	}
	if isNetworkError(context.Canceled) {
		t.Error("plain error should not be a network error")
	}
}

func TestImmutableBlobDedup(t *testing.T) {
	requireStack(t)
	ctx := context.Background()
	p := prov(t, models.PackageGeneric)
	// Two remotes serving identical content: the second store hits the
	// already-exists branch (blob content is deduplicated).
	for _, name := range []string{"eng-dedup-a", "eng-dedup-b"} {
		r := seed(t, genericRemote(name))
		res, err := testEngine.Fetch(ctx, r, "blob.bin", p)
		if err != nil {
			t.Fatalf("%s fetch: %v", name, err)
		}
		if readAll(t, res) != "immutable blob" {
			t.Errorf("%s content mismatch", name)
		}
	}
}

func TestCircuitBreakerStates(t *testing.T) {
	requireStack(t)
	ctx := context.Background()
	cb := NewCircuitBreaker(testCache)
	const key = "cb-states"
	if err := testCache.ResetCircuit(ctx, key); err != nil {
		t.Fatal(err)
	}

	if cb.IsOpen(ctx, key) {
		t.Error("fresh breaker should be closed")
	}
	if cb.Health(ctx, key).Status != "healthy" {
		t.Error("fresh breaker should be healthy")
	}
	cb.RecordFailure(ctx, key)
	if s := cb.Health(ctx, key).Status; s != "degraded" {
		t.Errorf("one failure should be degraded, got %q", s)
	}
	for i := 0; i < 6; i++ {
		cb.RecordFailure(ctx, key)
	}
	if !cb.IsOpen(ctx, key) {
		t.Error("breaker should be open after threshold failures")
	}
	if s := cb.Health(ctx, key).Status; s != "down" {
		t.Errorf("open breaker should be down, got %q", s)
	}
	cb.RecordSuccess(ctx, key)
	if cb.IsOpen(ctx, key) {
		t.Error("breaker should close after success")
	}
}

func asProxyError(err error, target **ProxyError) bool {
	pe, ok := err.(*ProxyError)
	if ok {
		*target = pe
	}
	return ok
}

// A dead object store degrades the cache to a pass-through instead of failing every miss: an index is
// served from the buffered upstream body and a blob is re-fetched and streamed, neither cached.
func TestStoreDownPassesThrough(t *testing.T) {
	requireStack(t)
	ctx := context.Background()
	minio, term, err := testsupport.StartMinio(ctx)
	if err != nil {
		t.Skip("minio unavailable")
	}
	var s3 *storage.S3
	for i := 0; i < 20; i++ {
		if s3, err = storage.NewS3(minio.Endpoint, minio.AccessKey, minio.SecretKey, "proxy-down", false, ""); err == nil {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	if err != nil {
		t.Fatal(err)
	}
	eng := NewEngine(testDB, testCache, s3)
	term() // the store goes away

	idx := seed(t, models.Remote{Name: "eng-down-npm", PackageType: models.PackageNPM, RepoType: models.RepoTypeRemote, BaseURL: upstream.URL})
	res, err := eng.Fetch(ctx, idx, "pkg", prov(t, models.PackageNPM))
	if err != nil {
		t.Fatalf("index fetch with store down: %v", err)
	}
	if body := readAll(t, res); res.Source != "remote" || !strings.Contains(body, `"name":"pkg"`) {
		t.Errorf("index: source=%s body=%q", res.Source, body)
	}
	if fresh, _ := testCache.CheckTTL(ctx, idx.Name, "pkg"); fresh {
		t.Error("an uncached index must not be marked fresh")
	}

	blob := seed(t, genericRemote("eng-down-blob"))
	res, err = eng.Fetch(ctx, blob, "blob.bin", prov(t, models.PackageGeneric))
	if err != nil {
		t.Fatalf("blob fetch with store down: %v", err)
	}
	if body := readAll(t, res); res.Source != "remote" || body != "immutable blob" {
		t.Errorf("blob: source=%s body=%q", res.Source, body)
	}
	if a, _ := testDB.GetArtifact(ctx, blob.Name, "blob.bin"); a != nil {
		t.Error("an uncached blob must not get an artifact row")
	}
}
