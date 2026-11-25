package storage

import (
	"context"
	"errors"
	"io"
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

// putStore answers every PUT with respond(attempt), counting attempts, and 404s every other request.
func putStore(t *testing.T, respond func(attempt int32, w http.ResponseWriter)) (*S3, *atomic.Int32) {
	t.Helper()
	var puts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut {
			if strings.Count(strings.Trim(r.URL.Path, "/"), "/") > 0 {
				w.WriteHeader(http.StatusNotFound)
			}
			return
		}
		_, _ = io.Copy(io.Discard, r.Body)
		respond(puts.Add(1), w)
	}))
	t.Cleanup(srv.Close)
	s, err := NewS3(strings.TrimPrefix(srv.URL, "http://"), "key", "secret", "bucket", false, "us-east-1")
	if err != nil {
		t.Fatal(err)
	}
	return s.RequestPath(), &puts
}

func TestUploadOnSlowHealthyStoreSucceeds(t *testing.T) {
	s, puts := putStore(t, func(attempt int32, w http.ResponseWriter) {
		if attempt == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		time.Sleep(RequestTimeout + 500*time.Millisecond)
		w.Header().Set("ETag", `"x"`)
	})

	if err := s.Upload(context.Background(), "blobs/x", strings.NewReader("payload"), 7, "text/plain"); err != nil {
		t.Fatalf("an upload outlasting the request timeout after a transient 503 must succeed: %v", err)
	}
	if n := puts.Load(); n != 2 {
		t.Errorf("PUT attempts = %d, want 2", n)
	}
	if !s.Healthy() {
		t.Error("a slow upload on a healthy store must not mark it unhealthy")
	}
}

func TestFailedUploadKeepsStoreHealthy(t *testing.T) {
	s, puts := putStore(t, func(_ int32, w http.ResponseWriter) { w.WriteHeader(http.StatusInternalServerError) })

	if err := s.Upload(context.Background(), "blobs/x", strings.NewReader("payload"), 7, "text/plain"); err == nil {
		t.Fatal("upload against a failing store succeeded")
	}
	if puts.Load() < 2 {
		t.Errorf("PUT attempts = %d, want the client to retry", puts.Load())
	}
	if !s.Healthy() {
		t.Error("a failed upload must not mark the store unhealthy")
	}
}

func TestUploadRefusedWhileUnhealthy(t *testing.T) {
	s, puts := putStore(t, func(_ int32, w http.ResponseWriter) { w.Header().Set("ETag", `"x"`) })
	s.health.set(errors.New("probe failed"))

	if err := s.Upload(context.Background(), "blobs/x", strings.NewReader("payload"), 7, "text/plain"); !errors.Is(err, ErrUnhealthy) {
		t.Fatalf("upload on an unhealthy store: err=%v, want ErrUnhealthy", err)
	}
	_, err := NewCAS(s).StoreOrSpool(context.Background(), strings.NewReader("payload"), "text/plain")
	var unavailable *StoreUnavailableError
	if !errors.As(err, &unavailable) {
		t.Fatalf("StoreOrSpool on an unhealthy store: want StoreUnavailableError, got %v", err)
	}
	_ = unavailable.Spool.Close()
	if n := puts.Load(); n != 0 {
		t.Errorf("PUTs on an unhealthy store = %d, want 0", n)
	}
}
