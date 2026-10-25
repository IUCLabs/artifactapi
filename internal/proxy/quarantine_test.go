package proxy

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/IUCLabs/artifactapi/internal/cache"
	"github.com/IUCLabs/artifactapi/pkg/models"
	goredis "github.com/redis/go-redis/v9"
)

func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}

func quarantineUpstream(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/fresh.tgz":
			w.Header().Set("Last-Modified", time.Now().Add(-time.Hour).UTC().Format(http.TimeFormat))
		case "/old.tgz":
			w.Header().Set("Last-Modified", time.Now().Add(-30*24*time.Hour).UTC().Format(http.TimeFormat))
		case "/future.tgz":
			w.Header().Set("Last-Modified", time.Now().Add(30*24*time.Hour).UTC().Format(http.TimeFormat))
		case "/undated.tgz", "/pkg":
		default:
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte("body " + r.URL.Path))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func quarantineRemote(t *testing.T, name string, enabled bool) models.Remote {
	r := models.Remote{Name: name, PackageType: models.PackageNPM, RepoType: models.RepoTypeRemote,
		BaseURL: quarantineUpstream(t).URL, QuarantineEnabled: enabled, QuarantineDays: 3}
	return seed(t, r)
}

func wantQuarantined(t *testing.T, err error) *ProxyError {
	t.Helper()
	var pe *ProxyError
	if !errors.As(err, &pe) || pe.Status != http.StatusForbidden || !Quarantined(err) {
		t.Fatalf("want quarantine 403, got %v", err)
	}
	return pe
}

func TestQuarantineBlocksFreshArtifact(t *testing.T) {
	requireStack(t)
	ctx := context.Background()
	r := quarantineRemote(t, "q-fresh", true)
	pe := wantQuarantined(t, func() error { _, err := testEngine.Fetch(ctx, r, "fresh.tgz", prov(t, models.PackageNPM)); return err }())
	if d := time.Until(pe.Available); d < 70*time.Hour || d > 72*time.Hour {
		t.Errorf("available in %s, want ~71h", d)
	}
	if a, _ := testDB.GetArtifact(ctx, r.Name, "fresh.tgz"); a != nil {
		t.Error("quarantined artifact was cached")
	}
	_, err := testEngine.Head(ctx, r, "fresh.tgz", prov(t, models.PackageNPM))
	_ = wantQuarantined(t, err)
}

func TestQuarantineServesOldArtifact(t *testing.T) {
	requireStack(t)
	r := quarantineRemote(t, "q-old", true)
	res, err := testEngine.Fetch(context.Background(), r, "old.tgz", prov(t, models.PackageNPM))
	if err != nil {
		t.Fatal(err)
	}
	if got := readAll(t, res); got != "body /old.tgz" {
		t.Errorf("body = %q", got)
	}
}

func TestQuarantineFallsBackToFirstSeen(t *testing.T) {
	requireStack(t)
	ctx := context.Background()
	r := quarantineRemote(t, "q-undated", true)
	p := prov(t, models.PackageNPM)
	_, err := testEngine.Fetch(ctx, r, "undated.tgz", p)
	first := wantQuarantined(t, err).Published
	if time.Since(first) > time.Minute {
		t.Fatalf("first seen %s, want now", first)
	}
	_, err = testEngine.Fetch(ctx, r, "undated.tgz", p)
	if again := wantQuarantined(t, err).Published; !again.Equal(first) {
		t.Errorf("first seen moved: %s -> %s", first, again)
	}

	r.QuarantineDays = 1
	c := goredis.NewClient(must(goredis.ParseURL(testRedis)))
	defer func() { _ = c.Close() }()
	if err := c.Set(ctx, "firstseen:q-undated:undated.tgz", time.Now().Add(-48*time.Hour).Unix(), time.Hour).Err(); err != nil {
		t.Fatal(err)
	}
	if _, err := testEngine.Fetch(ctx, r, "undated.tgz", p); err != nil {
		t.Errorf("seen 48h ago with 1 day quarantine: %v", err)
	}
}

func TestQuarantineSkipsMutable(t *testing.T) {
	requireStack(t)
	r := quarantineRemote(t, "q-mutable", true)
	if _, err := testEngine.Fetch(context.Background(), r, "pkg", prov(t, models.PackageNPM)); err != nil {
		t.Errorf("mutable index blocked: %v", err)
	}
}

func TestQuarantineDisabled(t *testing.T) {
	requireStack(t)
	r := quarantineRemote(t, "q-off", false)
	if _, err := testEngine.Fetch(context.Background(), r, "fresh.tgz", prov(t, models.PackageNPM)); err != nil {
		t.Errorf("disabled quarantine blocked: %v", err)
	}
}

func TestQuarantineKeepsServingCached(t *testing.T) {
	requireStack(t)
	ctx := context.Background()
	r := quarantineRemote(t, "q-cached", false)
	p := prov(t, models.PackageNPM)
	if _, err := testEngine.Fetch(ctx, r, "fresh.tgz", p); err != nil {
		t.Fatal(err)
	}
	r.QuarantineEnabled = true
	expire(t, r.Name, "fresh.tgz")
	if _, err := testEngine.Fetch(ctx, r, "fresh.tgz", p); err != nil {
		t.Errorf("artifact cached before quarantine refused on refetch: %v", err)
	}
}

func TestQuarantineCapsFutureLastModified(t *testing.T) {
	requireStack(t)
	r := quarantineRemote(t, "q-future", true)
	_, err := testEngine.Fetch(context.Background(), r, "future.tgz", prov(t, models.PackageNPM))
	pe := wantQuarantined(t, err)
	if d := time.Until(pe.Available); d < 71*time.Hour || d > 72*time.Hour {
		t.Errorf("available in %s, want ~72h", d)
	}
}

func TestQuarantineRedisErrorUnavailable(t *testing.T) {
	requireStack(t)
	ctx := context.Background()
	r := quarantineRemote(t, "q-redis-down", true)
	r.StaleOnError = true
	down := must(cache.NewRedis(testRedis))
	_ = down.Close()
	eng := NewEngine(testDB, down, testEngine.store)
	_, err := eng.Fetch(ctx, r, "undated.tgz", prov(t, models.PackageNPM))
	var pe *ProxyError
	if !errors.As(err, &pe) || pe.Status != http.StatusServiceUnavailable || pe.RetryAfter == 0 || Quarantined(err) {
		t.Fatalf("want 503 with Retry-After, got %#v", err)
	}
	if a, _ := testDB.GetArtifact(ctx, r.Name, "undated.tgz"); a != nil {
		t.Error("artifact cached while quarantine check unavailable")
	}
}

func TestQuarantineStaleOnErrorServesNothingUncached(t *testing.T) {
	requireStack(t)
	ctx := context.Background()
	r := quarantineRemote(t, "q-stale", true)
	r.StaleOnError = true
	p := prov(t, models.PackageNPM)
	_, err := testEngine.Fetch(ctx, r, "fresh.tgz", p)
	_ = wantQuarantined(t, err)
	r.BaseURL = "http://127.0.0.1:1"
	if _, err := testEngine.Fetch(ctx, r, "fresh.tgz", p); err == nil {
		t.Error("stale_on_error served an uncached quarantined path")
	}
	if a, _ := testDB.GetArtifact(ctx, r.Name, "fresh.tgz"); a != nil {
		t.Error("quarantined artifact was cached")
	}
}
