package gc

import (
	"bytes"
	"context"
	"os"
	"testing"
	"time"

	"github.com/IUCLabs/artifactapi/internal/database"
	"github.com/IUCLabs/artifactapi/internal/storage"
	"github.com/IUCLabs/artifactapi/internal/testsupport"
)

var (
	testDB    *database.DB
	testStore *storage.S3
)

func TestMain(m *testing.M) {
	ctx := context.Background()
	dsn, termPG, err := testsupport.StartPostgres(ctx)
	if err != nil {
		os.Exit(m.Run())
	}
	minio, termMinio, err := testsupport.StartMinio(ctx)
	if err != nil {
		termPG()
		os.Exit(m.Run())
	}
	db, err := database.New(dsn)
	if err != nil {
		panic(err)
	}
	var s3 *storage.S3
	for i := 0; i < 20; i++ {
		if s3, err = storage.NewS3(minio.Endpoint, minio.AccessKey, minio.SecretKey, "gc-test", false, ""); err == nil {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	if err != nil {
		panic(err)
	}
	testDB = db
	testStore = s3

	code := m.Run()
	db.Close()
	termMinio()
	termPG()
	if code != 0 {
		os.Exit(code)
	}
}

func TestSweepDeletesOldOrphan(t *testing.T) {
	if testDB == nil {
		t.Skip("Docker unavailable")
	}
	ctx := context.Background()
	hash := "sha256:gcorphan"
	key := storage.BlobKey("gcorphan")

	if err := testStore.Upload(ctx, key, bytes.NewReader([]byte("orphan")), 6, "application/octet-stream"); err != nil {
		t.Fatal(err)
	}
	if err := testDB.UpsertBlob(ctx, hash, key, 6, "application/octet-stream"); err != nil {
		t.Fatal(err)
	}
	// Age the blob past the grace period.
	if _, err := testDB.Pool.Exec(ctx, `UPDATE blobs SET created_at = now() - interval '2 hours' WHERE content_hash = $1`, hash); err != nil {
		t.Fatal(err)
	}

	c := New(testDB, testStore, time.Hour)
	c.sweep(ctx)

	if exists, _ := testStore.Exists(ctx, key); exists {
		t.Error("expected orphan object deleted from store")
	}
	orphans, _ := testDB.FindOrphanedBlobs(ctx, 0)
	for _, b := range orphans {
		if b.ContentHash == hash {
			t.Error("expected orphan blob row deleted")
		}
	}
}

func TestSweepNoOrphans(t *testing.T) {
	if testDB == nil {
		t.Skip("Docker unavailable")
	}
	// A sweep with nothing to collect should be a clean no-op.
	New(testDB, testStore, time.Hour).sweep(context.Background())
}

func TestRunStopsOnContextCancel(t *testing.T) {
	if testDB == nil {
		t.Skip("Docker unavailable")
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		New(testDB, testStore, time.Hour).Run(ctx)
		close(done)
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after context cancel")
	}
}
