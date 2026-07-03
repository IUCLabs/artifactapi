package v2

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/IUCLabs/artifactapi/internal/database"
	"github.com/IUCLabs/artifactapi/internal/testsupport"
)

var testDSN string

func TestMain(m *testing.M) {
	ctx := context.Background()
	dsn, terminate, err := testsupport.StartPostgres(ctx)
	if err != nil {
		os.Exit(m.Run())
	}
	testDSN = dsn
	code := m.Run()
	terminate()
	if code != 0 {
		os.Exit(code)
	}
}

// closedDB returns a DB whose pool has been closed, so every query fails —
// used to drive the handlers' error branches.
func closedDB(t *testing.T) *database.DB {
	t.Helper()
	if testDSN == "" {
		t.Skip("Docker unavailable")
	}
	db, err := database.New(testDSN)
	if err != nil {
		t.Fatalf("new db: %v", err)
	}
	db.Close()
	return db
}

func do(t *testing.T, h http.Handler, method, path, body string) int {
	t.Helper()
	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, r)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w.Code
}

func TestRemotesErrorPaths(t *testing.T) {
	h := NewRemotesHandler(closedDB(t)).Routes()
	if c := do(t, h, "GET", "/", ""); c != 500 {
		t.Errorf("list with dead db = %d, want 500", c)
	}
	if c := do(t, h, "POST", "/", `{"name":"x","package_type":"generic","repo_type":"remote","base_url":"https://x"}`); c != 500 {
		t.Errorf("create with dead db = %d, want 500", c)
	}
	if c := do(t, h, "PUT", "/x", `{"package_type":"generic","base_url":"https://x"}`); c != 500 {
		t.Errorf("update with dead db = %d, want 500", c)
	}
	if c := do(t, h, "GET", "/x", ""); c != 404 {
		t.Errorf("get missing = %d, want 404", c)
	}
	if c := do(t, h, "DELETE", "/x", ""); c != 500 {
		t.Errorf("delete with dead db = %d, want 500", c)
	}
	// Bad request bodies never reach the db.
	if c := do(t, h, "POST", "/", `not json`); c != 400 {
		t.Errorf("invalid json = %d, want 400", c)
	}
}

func TestVirtualsErrorPaths(t *testing.T) {
	h := NewVirtualsHandler(closedDB(t)).Routes()
	if c := do(t, h, "GET", "/", ""); c != 500 {
		t.Errorf("list = %d, want 500", c)
	}
	if c := do(t, h, "GET", "/x", ""); c != 404 {
		t.Errorf("get missing = %d, want 404", c)
	}
	if c := do(t, h, "POST", "/", `{"name":"v","package_type":"helm","members":["a"]}`); c != 500 {
		t.Errorf("create = %d, want 500", c)
	}
	if c := do(t, h, "PUT", "/v", `{"package_type":"helm","members":["a"]}`); c != 500 {
		t.Errorf("update = %d, want 500", c)
	}
	if c := do(t, h, "DELETE", "/v", ""); c != 500 {
		t.Errorf("delete = %d, want 500", c)
	}
}

func TestStatsErrorPaths(t *testing.T) {
	h := NewStatsHandler(closedDB(t)).Routes()
	for _, p := range []string{"/", "/top-remotes", "/top-files-by-hits", "/top-files-by-bandwidth"} {
		if c := do(t, h, "GET", p, ""); c != 500 {
			t.Errorf("stats %s = %d, want 500", p, c)
		}
	}
}

func TestLocalErrorPaths(t *testing.T) {
	h := NewLocalHandler(closedDB(t), nil).Routes()
	// GetRemote fails on the closed db -> not found.
	if c := do(t, h, "PUT", "/x/files/a.bin", "data"); c != 404 {
		t.Errorf("upload unknown repo = %d, want 404", c)
	}
	// download / remove hit the db and 500.
	if c := do(t, h, "GET", "/x/files/a.bin", ""); c != 500 {
		t.Errorf("download = %d, want 500", c)
	}
	if c := do(t, h, "DELETE", "/x/files/a.bin", ""); c != 500 {
		t.Errorf("remove = %d, want 500", c)
	}
}

func TestLocalHandlerDBAccessor(t *testing.T) {
	db := closedDB(t)
	if NewLocalHandler(db, nil).DB() != db {
		t.Error("DB() should return the handler's database")
	}
}
