package storage

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
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
	reader.Close()
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
	reader.Close()
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
