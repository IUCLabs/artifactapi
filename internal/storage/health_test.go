package storage

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const (
	storeOK int32 = iota
	storeDown
	storeHang
)

// fakeS3 answers bucket HEADs and 404s every object in storeOK mode, 503s everything in storeDown,
// and accepts but never answers in storeHang.
func fakeS3(t *testing.T) (*S3, *atomic.Int32) {
	t.Helper()
	var mode atomic.Int32
	stop := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch mode.Load() {
		case storeHang:
			select {
			case <-r.Context().Done():
			case <-stop:
			}
		case storeDown:
			w.WriteHeader(http.StatusServiceUnavailable)
		default:
			if strings.Count(strings.Trim(r.URL.Path, "/"), "/") > 0 {
				w.WriteHeader(http.StatusNotFound)
			}
		}
	}))
	t.Cleanup(func() { close(stop); srv.Close() })
	s, err := NewS3(strings.TrimPrefix(srv.URL, "http://"), "key", "secret", "bucket", false, "us-east-1")
	if err != nil {
		t.Fatal(err)
	}
	return s.RequestPath(), &mode
}

func TestHealthPassiveMarking(t *testing.T) {
	s, mode := fakeS3(t)
	ctx := context.Background()

	if exists, err := s.Exists(ctx, "blobs/x"); err != nil || exists || !s.Healthy() {
		t.Fatalf("a missing key must not mark the store unhealthy: exists=%v err=%v healthy=%v", exists, err, s.Healthy())
	}

	mode.Store(storeHang)
	cctx, cancel := context.WithCancel(ctx)
	time.AfterFunc(50*time.Millisecond, cancel)
	if _, err := s.Stat(cctx, "blobs/x"); err == nil || !s.Healthy() {
		t.Fatalf("the caller's cancellation must not mark the store unhealthy: err=%v healthy=%v", err, s.Healthy())
	}

	mode.Store(storeDown)
	if _, err := s.Stat(ctx, "blobs/x"); err == nil || s.Healthy() {
		t.Fatalf("a 503 must mark the store unhealthy: err=%v healthy=%v", err, s.Healthy())
	}

	mode.Store(storeOK)
	if _, err := s.Exists(ctx, "blobs/x"); err != nil || s.Healthy() {
		t.Fatalf("only a probe marks the store healthy again: err=%v healthy=%v", err, s.Healthy())
	}
}

func TestHealthProbe(t *testing.T) {
	s, mode := fakeS3(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.RunHealthProbe(ctx, 20*time.Millisecond, 200*time.Millisecond)

	waitHealthy := func(want bool) {
		t.Helper()
		deadline := time.Now().Add(2 * time.Second)
		for s.Healthy() != want {
			if time.Now().After(deadline) {
				t.Fatalf("healthy never became %v", want)
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
	mode.Store(storeDown)
	waitHealthy(false)
	mode.Store(storeOK)
	waitHealthy(true)
	mode.Store(storeHang)
	waitHealthy(false)
	mode.Store(storeOK)
	waitHealthy(true)
}

func TestRequestPathTimesOutOnHungStore(t *testing.T) {
	s, mode := fakeS3(t)
	mode.Store(storeHang)
	start := time.Now()
	_, err := s.Exists(context.Background(), "blobs/x")
	if elapsed := time.Since(start); err == nil || elapsed > RequestTimeout+time.Second {
		t.Fatalf("Exists on a hung store: err=%v after %s", err, elapsed)
	}
	if s.Healthy() {
		t.Error("a timed-out call must mark the store unhealthy")
	}
}
