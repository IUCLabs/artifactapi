package cache

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/IUCLabs/artifactapi/internal/testsupport"
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
	r.Close()
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
	ok, err := testRedis.AcquireLock(ctx, "r", "lockpath", time.Minute)
	if err != nil || !ok {
		t.Fatalf("first acquire should succeed: %v %v", ok, err)
	}
	if ok, _ := testRedis.AcquireLock(ctx, "r", "lockpath", time.Minute); ok {
		t.Error("second acquire should fail while held")
	}
	if err := testRedis.ReleaseLock(ctx, "r", "lockpath"); err != nil {
		t.Fatal(err)
	}
	if ok, _ := testRedis.AcquireLock(ctx, "r", "lockpath", time.Minute); !ok {
		t.Error("acquire should succeed after release")
	}
}

func TestETagAndToken(t *testing.T) {
	requireRedis(t)
	ctx := context.Background()
	if v, _ := testRedis.GetETag(ctx, "r", "missing"); v != "" {
		t.Error("missing etag should be empty")
	}
	testRedis.SetETag(ctx, "r", "p", `"abc"`, time.Minute)
	if v, _ := testRedis.GetETag(ctx, "r", "p"); v != `"abc"` {
		t.Errorf("etag = %q", v)
	}

	if v, _ := testRedis.GetToken(ctx, "missing"); v != "" {
		t.Error("missing token should be empty")
	}
	testRedis.SetToken(ctx, "key", "tok", time.Minute)
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
	testRedis.ResetCircuit(ctx, "cr")
	if n, _ := testRedis.GetCircuitFailures(ctx, "cr"); n != 0 {
		t.Errorf("failures after reset = %d", n)
	}
}

func TestFlushRemote(t *testing.T) {
	requireRedis(t)
	ctx := context.Background()
	testRedis.SetTTL(ctx, "flushme", "a", time.Hour)
	testRedis.SetETag(ctx, "flushme", "a", "x", time.Hour)
	if err := testRedis.FlushRemote(ctx, "flushme"); err != nil {
		t.Fatal(err)
	}
	if fresh, _ := testRedis.CheckTTL(ctx, "flushme", "a"); fresh {
		t.Error("expected keys flushed")
	}
}
