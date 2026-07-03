package v2

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/IUCLabs/artifactapi/internal/database"
	"github.com/IUCLabs/artifactapi/internal/storage"
	"github.com/IUCLabs/artifactapi/internal/testsupport"
	"github.com/IUCLabs/artifactapi/pkg/models"
)

// TestLocalUploadStoreFailure covers the upload handlers' store-error branches
// by killing the object store after a successful upload.
func TestLocalUploadStoreFailure(t *testing.T) {
	if testDSN == "" {
		t.Skip("Docker unavailable")
	}
	ctx := context.Background()
	db, err := database.New(testDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	conn, termMinio, err := testsupport.StartMinio(ctx)
	if err != nil {
		t.Skip("minio unavailable")
	}
	var store *storage.S3
	for i := 0; i < 20; i++ {
		if store, err = storage.NewS3(conn.Endpoint, conn.AccessKey, conn.SecretKey, "fault", false, ""); err == nil {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	if err != nil {
		termMinio()
		t.Fatal(err)
	}

	for _, pt := range []models.PackageType{models.PackageGeneric, models.PackagePyPI} {
		if err := db.CreateRemote(ctx, &models.Remote{Name: "fault-" + string(pt), PackageType: pt, RepoType: models.RepoTypeLocal}); err != nil {
			t.Fatal(err)
		}
	}

	h := NewLocalHandler(db, store)
	router := chi.NewRouter()
	router.Route("/remotes/{name}/files", func(r chi.Router) {
		r.Put("/*", h.Routes().ServeHTTP)
	})
	srv := httptest.NewServer(router)
	defer srv.Close()

	put := func(name, path, body string) int {
		rq, _ := http.NewRequest("PUT", srv.URL+"/remotes/"+name+"/files/"+path, strings.NewReader(body))
		resp, err := http.DefaultClient.Do(rq)
		if err != nil {
			t.Fatalf("put: %v", err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}

	// Sanity: uploads succeed while the store is up.
	if c := put("fault-generic", "ok.bin", "data"); c != 201 {
		t.Fatalf("generic upload while up = %d", c)
	}
	if c := put("fault-pypi", "foo-1.0-py3-none-any.whl", "wheel"); c != 201 {
		t.Fatalf("pypi upload while up = %d", c)
	}

	// Kill the store; subsequent CAS.Store calls fail -> 500.
	termMinio()
	if c := put("fault-generic", "after.bin", "data"); c != 500 {
		t.Errorf("generic upload after store down = %d, want 500", c)
	}
	if c := put("fault-pypi", "bar-1.0-py3-none-any.whl", "wheel"); c != 500 {
		t.Errorf("pypi upload after store down = %d, want 500", c)
	}
}
