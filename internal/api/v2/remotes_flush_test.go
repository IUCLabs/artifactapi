package v2

import (
	"context"
	"errors"
	"testing"

	"github.com/IUCLabs/artifactapi/internal/database"
	"github.com/IUCLabs/artifactapi/pkg/models"
)

// fakeFlusher records FlushRemote calls so a test can assert whether — and how
// often — a remote's cached metadata was purged.
type fakeFlusher struct {
	calls []string
	err   error
}

func (f *fakeFlusher) FlushRemote(_ context.Context, remote string) error {
	f.calls = append(f.calls, remote)
	return f.err
}

func seedRemote(t *testing.T, db *database.DB, name, baseURL string) {
	t.Helper()
	err := db.CreateRemote(context.Background(), &models.Remote{
		Name:        name,
		PackageType: models.PackageRPM,
		RepoType:    models.RepoTypeRemote,
		BaseURL:     baseURL,
	})
	if err != nil {
		t.Fatalf("seed remote: %v", err)
	}
}

// A base_url change must flush the remote's cached metadata exactly once, while
// an update that leaves base_url untouched must not flush at all.
func TestUpdateFlushesCacheOnBaseURLChange(t *testing.T) {
	if testDSN == "" {
		t.Skip("Docker unavailable")
	}
	db, err := database.New(testDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	const name = "rpm-flush-change"
	seedRemote(t, db, name, "https://old.example.com/repo")

	ff := &fakeFlusher{}
	h := NewRemotesHandler(db, ff, nil).Routes()

	if c := do(t, h, "PUT", "/"+name, `{"package_type":"rpm","repo_type":"remote","base_url":"https://new.example.com/repo"}`); c != 200 {
		t.Fatalf("update (backend change) = %d, want 200", c)
	}
	if len(ff.calls) != 1 || ff.calls[0] != name {
		t.Fatalf("flush calls = %v, want exactly one flush of %q", ff.calls, name)
	}

	// Re-updating with the same (now current) base_url must not flush again.
	ff.calls = nil
	if c := do(t, h, "PUT", "/"+name, `{"package_type":"rpm","repo_type":"remote","base_url":"https://new.example.com/repo"}`); c != 200 {
		t.Fatalf("update (no backend change) = %d, want 200", c)
	}
	if len(ff.calls) != 0 {
		t.Fatalf("flush calls = %v, want no flush when base_url is unchanged", ff.calls)
	}
}

// A flush error must be swallowed: the DB update already succeeded, so the
// request still returns 200.
func TestUpdateFlushFailureStillSucceeds(t *testing.T) {
	if testDSN == "" {
		t.Skip("Docker unavailable")
	}
	db, err := database.New(testDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	const name = "rpm-flush-error"
	seedRemote(t, db, name, "https://old.example.com/repo")

	ff := &fakeFlusher{err: errors.New("redis down")}
	h := NewRemotesHandler(db, ff, nil).Routes()

	if c := do(t, h, "PUT", "/"+name, `{"package_type":"rpm","repo_type":"remote","base_url":"https://new.example.com/repo"}`); c != 200 {
		t.Fatalf("update with failing flush = %d, want 200", c)
	}
	if len(ff.calls) != 1 {
		t.Fatalf("flush calls = %v, want exactly one attempted flush", ff.calls)
	}
}
