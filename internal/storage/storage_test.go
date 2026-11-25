package storage

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"testing/iotest"
	"time"

	"github.com/minio/minio-go/v7"

	"github.com/IUCLabs/artifactapi/internal/testsupport"
)

var (
	testS3       *S3
	testEndpoint string
)

func TestMain(m *testing.M) {
	ctx := context.Background()
	conn, terminate, err := testsupport.StartMinio(ctx)
	if err != nil {
		os.Exit(m.Run())
	}
	var s3 *S3
	for i := 0; i < 20; i++ { // MinIO can report ready before bucket ops succeed
		if s3, err = NewS3(conn.Endpoint, conn.AccessKey, conn.SecretKey, "test-bucket", false, ""); err == nil {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	if err != nil {
		terminate()
		panic(err)
	}
	testS3 = s3
	testEndpoint = conn.Endpoint
	code := m.Run()
	terminate()
	if code != 0 {
		os.Exit(code)
	}
}

func requireS3(t *testing.T) {
	t.Helper()
	if testS3 == nil {
		t.Skip("Docker unavailable; skipping storage integration test")
	}
}

func TestKeys(t *testing.T) {
	if BlobKey("abc") != "blobs/sha256/abc" {
		t.Error("BlobKey")
	}
	if IndexKey("remote", "path/to/x") != "indexes/remote/path/to/x" {
		t.Error("IndexKey")
	}
}

func TestS3RoundTrip(t *testing.T) {
	requireS3(t)
	ctx := context.Background()
	key := "blobs/sha256/test1"
	content := []byte("hello storage")

	if err := testS3.Upload(ctx, key, bytes.NewReader(content), int64(len(content)), "text/plain"); err != nil {
		t.Fatalf("upload: %v", err)
	}

	exists, err := testS3.Exists(ctx, key)
	if err != nil || !exists {
		t.Fatalf("exists after upload: %v %v", exists, err)
	}

	reader, info, err := testS3.Download(ctx, key)
	if err != nil {
		t.Fatalf("download: %v", err)
	}
	got, _ := io.ReadAll(reader)
	_ = reader.Close()
	if !bytes.Equal(got, content) {
		t.Errorf("content mismatch: %q", got)
	}
	if info.Size != int64(len(content)) || info.ContentType != "text/plain" {
		t.Errorf("stat info wrong: size=%d ct=%s", info.Size, info.ContentType)
	}

	if _, err := testS3.Stat(ctx, key); err != nil {
		t.Errorf("stat: %v", err)
	}

	if err := testS3.Delete(ctx, key); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if exists, _ := testS3.Exists(ctx, key); exists {
		t.Error("expected object gone after delete")
	}
}

func TestNewS3ExistingBucket(t *testing.T) {
	requireS3(t)
	// The bucket already exists from TestMain, so ensureBucket takes the
	// "already present" path.
	if _, err := NewS3(testEndpoint, "minioadmin", "minioadmin", "test-bucket", false, ""); err != nil {
		t.Fatalf("second NewS3: %v", err)
	}
}

func TestS3DownloadMissing(t *testing.T) {
	requireS3(t)
	if _, _, err := testS3.Download(context.Background(), "does/not/exist"); err == nil {
		t.Error("expected error downloading missing key")
	}
	if _, err := testS3.Stat(context.Background(), "does/not/exist"); err == nil {
		t.Error("expected error stat-ing missing key")
	}
	if exists, err := testS3.Exists(context.Background(), "does/not/exist"); err != nil || exists {
		t.Errorf("Exists(missing) = %v, %v; want false, nil", exists, err)
	}
}

func TestCASStore(t *testing.T) {
	requireS3(t)
	ctx := context.Background()
	cas := NewCAS(testS3)
	content := "content-addressed payload"

	res, err := cas.Store(ctx, strings.NewReader(content), "text/plain")
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	if res.AlreadyExists {
		t.Error("first store should not report AlreadyExists")
	}
	if res.SizeBytes != int64(len(content)) || !strings.HasPrefix(res.ContentHash, "sha256:") {
		t.Errorf("unexpected result: %+v", res)
	}

	// Storing identical content again is deduplicated.
	res2, err := cas.Store(ctx, strings.NewReader(content), "text/plain")
	if err != nil {
		t.Fatalf("store again: %v", err)
	}
	if !res2.AlreadyExists || res2.ContentHash != res.ContentHash {
		t.Errorf("second store should dedup: %+v", res2)
	}

	// The stored blob is retrievable.
	reader, _, err := testS3.Download(ctx, res.S3Key)
	if err != nil {
		t.Fatalf("download stored blob: %v", err)
	}
	got, _ := io.ReadAll(reader)
	_ = reader.Close()
	if string(got) != content {
		t.Errorf("stored content mismatch: %q", got)
	}
}

// The fake endpoint fails every list but accepts every delete, as an S3 that
// tolerates deleting an empty key would.
func TestDeletePrefixReturnsListError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			_, _ = io.WriteString(w, `<DeleteResult></DeleteResult>`)
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, `<Error><Code>InternalError</Code><Message>list failed</Message></Error>`)
	}))
	defer srv.Close()
	client, err := minio.New(strings.TrimPrefix(srv.URL, "http://"), &minio.Options{Region: "us-east-1", MaxRetries: 1})
	if err != nil {
		t.Fatal(err)
	}
	s := &S3{client: client, bucket: "bucket"}
	err = s.DeletePrefix(context.Background(), "indexes/r/")
	if err == nil {
		t.Fatal("DeletePrefix on a failing list returned nil")
	}
}

// downS3 reports every key missing and rejects every write, so Store reaches the upload and fails there.
func downS3(t *testing.T) *S3 {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	t.Cleanup(srv.Close)
	client, err := minio.New(strings.TrimPrefix(srv.URL, "http://"), &minio.Options{Region: "us-east-1", MaxRetries: 1})
	if err != nil {
		t.Fatal(err)
	}
	return &S3{client: client, bucket: "bucket"}
}

func TestCASStoreOrSpoolStoreDown(t *testing.T) {
	content := "hashed before the store failed"
	_, err := NewCAS(downS3(t)).StoreOrSpool(context.Background(), strings.NewReader(content), "text/plain")
	var unavailable *StoreUnavailableError
	if !errors.As(err, &unavailable) || !errors.Is(err, ErrStoreUnavailable) {
		t.Fatalf("want StoreUnavailableError, got %v", err)
	}
	if unavailable.Size != int64(len(content)) {
		t.Errorf("size = %d, want %d", unavailable.Size, len(content))
	}
	got, _ := io.ReadAll(unavailable.Spool)
	if string(got) != content {
		t.Errorf("spool = %q, want %q", got, content)
	}
	name := unavailable.Spool.(interface{ Name() string }).Name()
	if err := unavailable.Spool.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(name); !os.IsNotExist(err) {
		t.Errorf("spool file %s survived Close: %v", name, err)
	}
}

// isolateTempDir points os.CreateTemp at a fresh directory and fails the test if anything is left in it.
func isolateTempDir(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("TMPDIR", dir)
	t.Cleanup(func() {
		if entries, _ := os.ReadDir(dir); len(entries) != 0 {
			t.Errorf("temp files left behind: %v", entries)
		}
	})
}

func TestCASStoreDownReturnsSentinelWithoutSpool(t *testing.T) {
	isolateTempDir(t)
	_, err := NewCAS(downS3(t)).Store(context.Background(), strings.NewReader("x"), "text/plain")
	if !errors.Is(err, ErrStoreUnavailable) {
		t.Fatalf("want ErrStoreUnavailable, got %v", err)
	}
	var unavailable *StoreUnavailableError
	if errors.As(err, &unavailable) && unavailable.Spool != nil {
		t.Error("Store must not hand back its closed spool")
	}
}

func TestCASStoreOrSpoolCancelledIsNotStoreUnavailable(t *testing.T) {
	isolateTempDir(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := NewCAS(downS3(t)).StoreOrSpool(ctx, strings.NewReader("x"), "text/plain")
	if !errors.Is(err, context.Canceled) || errors.Is(err, ErrStoreUnavailable) {
		t.Fatalf("want a plain context.Canceled, got %v", err)
	}
}

func TestCASStoreOrSpoolSourceErrorIsNotStoreUnavailable(t *testing.T) {
	isolateTempDir(t)
	src := io.MultiReader(strings.NewReader("partial"), iotest.ErrReader(io.ErrUnexpectedEOF))
	_, err := NewCAS(downS3(t)).StoreOrSpool(context.Background(), src, "text/plain")
	if !errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, ErrStoreUnavailable) {
		t.Fatalf("want a plain source error, got %v", err)
	}
}

// hungS3 is a request-path view of a store that never answers.
func hungS3(t *testing.T, timeout time.Duration) *S3 {
	t.Helper()
	stop := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-stop:
		}
	}))
	t.Cleanup(func() { close(stop); srv.Close() })
	client, err := minio.New(strings.TrimPrefix(srv.URL, "http://"), &minio.Options{Region: "us-east-1", MaxRetries: 1})
	if err != nil {
		t.Fatal(err)
	}
	return &S3{client: client, fast: client, bucket: "bucket", metaTimeout: timeout}
}

func TestDownloadHeadDeadline(t *testing.T) {
	const timeout = 200 * time.Millisecond
	s := hungS3(t, timeout)

	start := time.Now()
	_, _, err := s.Download(context.Background(), "k")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("hung head: err = %v, want DeadlineExceeded", err)
	}
	if elapsed := time.Since(start); elapsed > timeout+time.Second {
		t.Errorf("hung head took %s, want about %s", elapsed, timeout)
	}

	s.metaTimeout = 5 * time.Second
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, _, err = s.Download(ctx, "k")
	if !errors.Is(err, context.DeadlineExceeded) || ctx.Err() == nil {
		t.Fatalf("caller deadline: err = %v", err)
	}

	ctx, cancel = context.WithCancel(context.Background())
	time.AfterFunc(100*time.Millisecond, cancel)
	if _, _, err = s.Download(ctx, "k"); !errors.Is(err, context.Canceled) {
		t.Fatalf("caller cancel: err = %v, want Canceled", err)
	}
}

func TestDownloadBodyOutlivesHeadDeadline(t *testing.T) {
	requireS3(t)
	ctx := context.Background()
	key := "blobs/sha256/outlives-deadline"
	content := bytes.Repeat([]byte("x"), 8<<20)
	if err := testS3.Upload(ctx, key, bytes.NewReader(content), int64(len(content)), "application/octet-stream"); err != nil {
		t.Fatal(err)
	}
	s := testS3.RequestPath()
	s.metaTimeout = 100 * time.Millisecond
	r, _, err := s.Download(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(3 * s.metaTimeout)
	got, err := io.ReadAll(r)
	if err != nil || len(got) != len(content) {
		t.Fatalf("body after the head deadline: %d bytes, %v", len(got), err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestCancelOnCloseCancels(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	_ = cancelOnClose{cancel: cancel}.Close()
	if ctx.Err() == nil {
		t.Error("Close must cancel the download context")
	}
}

func TestProbe(t *testing.T) {
	requireS3(t)
	ctx := context.Background()
	if err := testS3.Probe(ctx); err != nil {
		t.Fatalf("probe on a live bucket: %v", err)
	}
	missing := *testS3
	missing.bucket = "probe-missing-bucket"
	if err := missing.Probe(ctx); err == nil || !strings.Contains(err.Error(), "missing") {
		t.Errorf("probe on a missing bucket: %v", err)
	}
	pctx, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancel()
	if err := hungS3(t, 0).Probe(pctx); err == nil {
		t.Error("probe on a hung store must fail")
	}
}

func TestRequestPathMakesOneAttempt(t *testing.T) {
	var keyRequests atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/bucket" || r.URL.Path == "/bucket/" {
			return
		}
		keyRequests.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	t.Cleanup(srv.Close)
	s3, err := NewS3(strings.TrimPrefix(srv.URL, "http://"), "key", "secret", "bucket", false, "us-east-1")
	if err != nil {
		t.Fatal(err)
	}
	s := s3.RequestPath()

	ops := map[string]func() error{
		"Stat":     func() error { _, err := s.Stat(context.Background(), "k"); return err },
		"Exists":   func() error { _, err := s.Exists(context.Background(), "k"); return err },
		"Download": func() error { _, _, err := s.Download(context.Background(), "k"); return err },
	}
	for name, op := range ops {
		keyRequests.Store(0)
		if err := op(); err == nil {
			t.Errorf("%s on a 5xx store: want an error", name)
		}
		if n := keyRequests.Load(); n != 1 {
			t.Errorf("%s on a 5xx store made %d requests, want 1", name, n)
		}
	}
}
