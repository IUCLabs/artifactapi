package proxy

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/IUCLabs/artifactapi/pkg/models"
)

// TestFetchMultiBaseURLRoundRobin drives distinct artifact paths through a
// remote configured with two upstreams and asserts both receive traffic.
func TestFetchMultiBaseURLRoundRobin(t *testing.T) {
	requireStack(t)
	ctx := context.Background()

	var hitsA, hitsB atomic.Int64
	upA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hitsA.Add(1)
		_, _ = w.Write([]byte("A"))
	}))
	defer upA.Close()
	upB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hitsB.Add(1)
		_, _ = w.Write([]byte("B"))
	}))
	defer upB.Close()

	r := seed(t, models.Remote{
		Name:         "eng-rr",
		PackageType:  models.PackageGeneric,
		RepoType:     models.RepoTypeRemote,
		BaseURL:      upA.URL,
		Mirrorlist:   []string{upB.URL},
		StaleOnError: true,
	})
	p := prov(t, models.PackageGeneric)

	const n = 10
	for i := 0; i < n; i++ {
		res, err := testEngine.Fetch(ctx, r, fmt.Sprintf("rr-%d.bin", i), p)
		if err != nil {
			t.Fatalf("fetch %d: %v", i, err)
		}
		_ = res.Reader.Close()
	}

	if hitsA.Load() == 0 || hitsB.Load() == 0 {
		t.Fatalf("round-robin did not spread across both upstreams: A=%d B=%d", hitsA.Load(), hitsB.Load())
	}
	if total := hitsA.Load() + hitsB.Load(); total != n {
		t.Fatalf("expected %d upstream hits total, got %d (A=%d B=%d)", n, total, hitsA.Load(), hitsB.Load())
	}
}

// TestFetchMultiBaseURLFailover asserts that a dead/erroring primary mirror
// transparently fails over to a healthy secondary, for both a 5xx primary and a
// network-unreachable primary.
func TestFetchMultiBaseURLFailover(t *testing.T) {
	requireStack(t)
	ctx := context.Background()

	var hitsB atomic.Int64
	upB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hitsB.Add(1)
		_, _ = w.Write([]byte("served-by-B"))
	}))
	defer upB.Close()
	up500 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer up500.Close()

	p := prov(t, models.PackageGeneric)

	// Primary returns 5xx: every request must still succeed via the secondary.
	r5xx := seed(t, models.Remote{
		Name:        "eng-failover-5xx",
		PackageType: models.PackageGeneric,
		RepoType:    models.RepoTypeRemote,
		BaseURL:     up500.URL,
		Mirrorlist:  []string{upB.URL},
	})
	for i := 0; i < 6; i++ {
		res, err := testEngine.Fetch(ctx, r5xx, fmt.Sprintf("fo5-%d.bin", i), p)
		if err != nil {
			t.Fatalf("5xx failover fetch %d: %v", i, err)
		}
		if got := readAll(t, res); got != "served-by-B" {
			t.Fatalf("5xx failover fetch %d body=%q, want served-by-B", i, got)
		}
	}

	// Primary is network-unreachable: failover must still reach the secondary.
	rNet := seed(t, models.Remote{
		Name:        "eng-failover-net",
		PackageType: models.PackageGeneric,
		RepoType:    models.RepoTypeRemote,
		BaseURL:     "http://127.0.0.1:1",
		Mirrorlist:  []string{upB.URL},
	})
	res, err := testEngine.Fetch(ctx, rNet, "fonet.bin", p)
	if err != nil {
		t.Fatalf("network failover fetch: %v", err)
	}
	if got := readAll(t, res); got != "served-by-B" {
		t.Fatalf("network failover body=%q, want served-by-B", got)
	}
	if hitsB.Load() == 0 {
		t.Fatal("secondary upstream never served during failover")
	}
}

// TestFetchDefinitiveStatusNoFailover asserts a definitive 404 from the first
// mirror is returned as-is (not failed over): a missing artifact is not a mirror
// outage. The remote is fresh so its round-robin cursor starts at index 0.
func TestFetchDefinitiveStatusNoFailover(t *testing.T) {
	requireStack(t)
	ctx := context.Background()

	var hitsB atomic.Int64
	up404 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer up404.Close()
	upB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hitsB.Add(1)
		_, _ = w.Write([]byte("B"))
	}))
	defer upB.Close()

	r := seed(t, models.Remote{
		Name:        "eng-no-failover-404",
		PackageType: models.PackageGeneric,
		RepoType:    models.RepoTypeRemote,
		BaseURL:     up404.URL,
		Mirrorlist:  []string{upB.URL},
	})
	_, err := testEngine.Fetch(ctx, r, "missing.bin", prov(t, models.PackageGeneric))
	var pe *ProxyError
	if err == nil || !asProxyError(err, &pe) || pe.Status != http.StatusNotFound {
		t.Fatalf("expected 404 ProxyError without failover, got %v", err)
	}
	if hitsB.Load() != 0 {
		t.Fatalf("404 from primary must not fail over, but secondary was hit %d times", hitsB.Load())
	}
}

// TestFetchSingleBaseURLUnchanged asserts a single-URL remote behaves exactly as
// before: one healthy URL succeeds, and one dead URL errors with no failover.
func TestFetchSingleBaseURLUnchanged(t *testing.T) {
	requireStack(t)
	ctx := context.Background()

	upB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("solo"))
	}))
	defer upB.Close()

	p := prov(t, models.PackageGeneric)

	rOK := seed(t, models.Remote{
		Name:        "eng-solo",
		PackageType: models.PackageGeneric,
		RepoType:    models.RepoTypeRemote,
		BaseURL:     upB.URL,
	})
	res, err := testEngine.Fetch(ctx, rOK, "solo.bin", p)
	if err != nil {
		t.Fatalf("single-url fetch: %v", err)
	}
	if got := readAll(t, res); got != "solo" {
		t.Fatalf("single-url body=%q, want solo", got)
	}

	rDead := seed(t, models.Remote{
		Name:        "eng-solo-dead",
		PackageType: models.PackageGeneric,
		RepoType:    models.RepoTypeRemote,
		BaseURL:     "http://127.0.0.1:1",
	})
	if _, err := testEngine.Fetch(ctx, rDead, "x.bin", p); err == nil {
		t.Fatal("single dead upstream should error, not succeed")
	}
}
