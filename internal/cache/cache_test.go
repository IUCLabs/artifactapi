package cache

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/IUCLabs/artifactapi/internal/testsupport"
	"github.com/redis/go-redis/v9"
)

var testRedis *Redis

func TestMain(m *testing.M) {
	ctx := context.Background()
	url, terminate, err := testsupport.StartRedis(ctx)
	if err != nil {
		os.Exit(m.Run())
	}
	r, err := NewRedis(url)
	if err != nil {
		terminate()
		panic(err)
	}
	testRedis = r
	code := m.Run()
	_ = r.Close()
	terminate()
	if code != 0 {
		os.Exit(code)
	}
}

func requireRedis(t *testing.T) {
	t.Helper()
	if testRedis == nil {
		t.Skip("Docker unavailable; skipping cache integration test")
	}
}

func TestNewRedisInvalid(t *testing.T) {
	if _, err := NewRedis("://bad-url"); err == nil {
		t.Error("expected error for invalid redis URL")
	}
}

func TestTTL(t *testing.T) {
	requireRedis(t)
	ctx := context.Background()
	if fresh, _ := testRedis.CheckTTL(ctx, "r", "missing"); fresh {
		t.Error("missing key should not be fresh")
	}
	if err := testRedis.SetTTL(ctx, "r", "p", time.Minute); err != nil {
		t.Fatal(err)
	}
	if fresh, err := testRedis.CheckTTL(ctx, "r", "p"); err != nil || !fresh {
		t.Errorf("expected fresh after SetTTL: %v %v", fresh, err)
	}
}

func TestLock(t *testing.T) {
	requireRedis(t)
	ctx := context.Background()
	token, err := testRedis.AcquireLock(ctx, "r", "lockpath", time.Minute)
	if err != nil || token == "" {
		t.Fatalf("first acquire should succeed: %q %v", token, err)
	}
	if again, _ := testRedis.AcquireLock(ctx, "r", "lockpath", time.Minute); again != "" {
		t.Error("second acquire should fail while held")
	}
	if err := testRedis.ReleaseLock(ctx, "r", "lockpath", "not-the-owner"); err != nil {
		t.Fatal(err)
	}
	if again, _ := testRedis.AcquireLock(ctx, "r", "lockpath", time.Minute); again != "" {
		t.Error("release with a foreign token must not drop the lock")
	}
	if err := testRedis.ReleaseLock(ctx, "r", "lockpath", token); err != nil {
		t.Fatal(err)
	}
	next, _ := testRedis.AcquireLock(ctx, "r", "lockpath", time.Minute)
	if next == "" {
		t.Fatal("acquire should succeed after release")
	}
	if next == token {
		t.Error("tokens must differ between holders")
	}
}

func TestStaleReleaseKeepsNextHolder(t *testing.T) {
	requireRedis(t)
	ctx := context.Background()
	stale, err := testRedis.AcquireLock(ctx, "r", "stale", 50*time.Millisecond)
	if err != nil || stale == "" {
		t.Fatalf("acquire: %q %v", stale, err)
	}
	time.Sleep(100 * time.Millisecond)
	fresh, err := testRedis.AcquireLock(ctx, "r", "stale", time.Minute)
	if err != nil || fresh == "" {
		t.Fatalf("acquire after expiry: %q %v", fresh, err)
	}
	if err := testRedis.ReleaseLock(ctx, "r", "stale", stale); err != nil {
		t.Fatal(err)
	}
	if again, _ := testRedis.AcquireLock(ctx, "r", "stale", time.Minute); again != "" {
		t.Fatal("stale holder's release dropped the second holder's lock")
	}
	if err := testRedis.ReleaseLock(ctx, "r", "stale", fresh); err != nil {
		t.Fatal(err)
	}
	if again, _ := testRedis.AcquireLock(ctx, "r", "stale", time.Minute); again == "" {
		t.Error("owner release should free the lock")
	}
}

func TestETagAndToken(t *testing.T) {
	requireRedis(t)
	ctx := context.Background()
	if v, _ := testRedis.GetETag(ctx, "r", "missing"); v != "" {
		t.Error("missing etag should be empty")
	}
	if err := testRedis.SetETag(ctx, "r", "p", `"abc"`, time.Minute); err != nil {
		t.Fatal(err)
	}
	if v, _ := testRedis.GetETag(ctx, "r", "p"); v != `"abc"` {
		t.Errorf("etag = %q", v)
	}
	if err := testRedis.SetETag(ctx, "r", "p", "", 0); err != nil {
		t.Fatal(err)
	}
	if v, _ := testRedis.GetETag(ctx, "r", "p"); v != "" {
		t.Errorf("empty etag should drop the stored one, got %q", v)
	}

	if v, _ := testRedis.GetToken(ctx, "missing"); v != "" {
		t.Error("missing token should be empty")
	}
	if err := testRedis.SetToken(ctx, "key", "tok", time.Minute); err != nil {
		t.Fatal(err)
	}
	if v, _ := testRedis.GetToken(ctx, "key"); v != "tok" {
		t.Errorf("token = %q", v)
	}
}

func TestCircuit(t *testing.T) {
	requireRedis(t)
	ctx := context.Background()
	if n, _ := testRedis.GetCircuitFailures(ctx, "cr"); n != 0 {
		t.Errorf("initial failures = %d", n)
	}
	n1, err := testRedis.IncrCircuitFailure(ctx, "cr", time.Minute)
	if err != nil || n1 != 1 {
		t.Fatalf("first incr = %d %v", n1, err)
	}
	n2, _ := testRedis.IncrCircuitFailure(ctx, "cr", time.Minute)
	if n2 != 2 {
		t.Errorf("second incr = %d", n2)
	}
	if n, _ := testRedis.GetCircuitFailures(ctx, "cr"); n != 2 {
		t.Errorf("get failures = %d", n)
	}
	if err := testRedis.ResetCircuit(ctx, "cr"); err != nil {
		t.Fatal(err)
	}
	if n, _ := testRedis.GetCircuitFailures(ctx, "cr"); n != 0 {
		t.Errorf("failures after reset = %d", n)
	}
}

func TestFlushRemote(t *testing.T) {
	requireRedis(t)
	ctx := context.Background()
	if err := testRedis.SetTTL(ctx, "flushme", "a", time.Hour); err != nil {
		t.Fatal(err)
	}
	if err := testRedis.SetETag(ctx, "flushme", "a", "x", time.Hour); err != nil {
		t.Fatal(err)
	}
	if err := testRedis.FlushRemote(ctx, "flushme"); err != nil {
		t.Fatal(err)
	}
	if fresh, _ := testRedis.CheckTTL(ctx, "flushme", "a"); fresh {
		t.Error("expected keys flushed")
	}
}

func setPathKeys(t *testing.T, remote string, paths ...string) {
	t.Helper()
	ctx := context.Background()
	for _, p := range paths {
		if err := testRedis.SetTTL(ctx, remote, p, time.Minute); err != nil {
			t.Fatal(err)
		}
		if err := testRedis.SetETag(ctx, remote, p, `"e"`, time.Minute); err != nil {
			t.Fatal(err)
		}
	}
}

func pathKeysExist(t *testing.T, remote, path string) (ttl, etag bool) {
	t.Helper()
	ctx := context.Background()
	ttl, _ = testRedis.CheckTTL(ctx, remote, path)
	e, _ := testRedis.GetETag(ctx, remote, path)
	return ttl, e != ""
}

func TestForgetPath(t *testing.T) {
	requireRedis(t)
	const meta = `repo/a*b?[c]\d.xml`
	setPathKeys(t, "fp", meta, "repo/aXb.xml")
	setPathKeys(t, "fp-other", meta)

	if err := testRedis.ForgetPath(context.Background(), "fp", meta); err != nil {
		t.Fatal(err)
	}
	if ttl, etag := pathKeysExist(t, "fp", meta); ttl || etag {
		t.Errorf("forgotten path keys remain: ttl=%v etag=%v", ttl, etag)
	}
	for _, k := range [][2]string{{"fp", "repo/aXb.xml"}, {"fp-other", meta}} {
		if ttl, etag := pathKeysExist(t, k[0], k[1]); !ttl || !etag {
			t.Errorf("%s:%s lost keys: ttl=%v etag=%v", k[0], k[1], ttl, etag)
		}
	}
}

func TestForgetPrefix(t *testing.T) {
	requireRedis(t)
	const prefix = `r*[1]?\/`
	under := []string{prefix + "repomd.xml", prefix + "sub/x.rpm"}
	// Each would match the prefix if its glob metacharacters were left unescaped.
	globMatches := []string{`rX11/repomd.xml`, `r[1]?\/x`, `r*1Z/x`}
	setPathKeys(t, "fx", append(under, globMatches...)...)
	setPathKeys(t, "fx-other", under...)

	if err := testRedis.ForgetPrefix(context.Background(), "fx", prefix); err != nil {
		t.Fatal(err)
	}
	for _, p := range under {
		if ttl, etag := pathKeysExist(t, "fx", p); ttl || etag {
			t.Errorf("%s keys remain: ttl=%v etag=%v", p, ttl, etag)
		}
	}
	for _, p := range globMatches {
		if ttl, etag := pathKeysExist(t, "fx", p); !ttl || !etag {
			t.Errorf("%s outside prefix lost keys: ttl=%v etag=%v", p, ttl, etag)
		}
	}
	for _, p := range under {
		if ttl, etag := pathKeysExist(t, "fx-other", p); !ttl || !etag {
			t.Errorf("other remote %s lost keys: ttl=%v etag=%v", p, ttl, etag)
		}
	}
}

func TestStoreHealth(t *testing.T) {
	requireRedis(t)
	ctx := context.Background()
	t.Cleanup(func() { _ = testRedis.client.Del(ctx, storeHealthKey).Err() })

	if !testRedis.StoreUp(ctx) {
		t.Error("a missing key must read as up")
	}
	if err := testRedis.SetStoreHealth(ctx, false, time.Minute); err != nil {
		t.Fatal(err)
	}
	if testRedis.StoreUp(ctx) {
		t.Error("down must read as down")
	}
	if ttl := testRedis.client.PTTL(ctx, storeHealthKey).Val(); ttl <= 0 || ttl > time.Minute {
		t.Errorf("health key TTL = %s, want (0, 1m]", ttl)
	}
	if err := testRedis.SetStoreHealth(ctx, true, time.Minute); err != nil {
		t.Fatal(err)
	}
	if !testRedis.StoreUp(ctx) {
		t.Error("up must read as up")
	}

	if err := testRedis.SetStoreHealth(ctx, false, 50*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	if !testRedis.StoreUp(ctx) {
		t.Error("an expired down key must read as up")
	}
}

func TestStoreUpOnRedisError(t *testing.T) {
	requireRedis(t)
	ctx := context.Background()
	if err := testRedis.SetStoreHealth(ctx, false, time.Minute); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = testRedis.client.Del(ctx, storeHealthKey).Err() })
	closed := &Redis{client: redis.NewClient(testRedis.client.Options())}
	_ = closed.Close()
	if !closed.StoreUp(ctx) {
		t.Error("a Redis error must read as up")
	}
}
