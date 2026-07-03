package v2

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/IUCLabs/artifactapi/internal/database"
	"github.com/IUCLabs/artifactapi/pkg/models"
)

// TestLocalObjectsListing verifies that files uploaded to a local repo (which
// live in local_files, not artifacts) are listed by the local objects endpoint
// and can be evicted through it.
func TestLocalObjectsListing(t *testing.T) {
	if testDSN == "" {
		t.Skip("Docker unavailable")
	}
	ctx := context.Background()
	db, err := database.New(testDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	const repo = "rpm-local-objs"
	if err := db.CreateRemote(ctx, &models.Remote{Name: repo, PackageType: models.PackageRPM, RepoType: models.RepoTypeLocal}); err != nil {
		t.Fatal(err)
	}

	const hash = "sha256:aa11"
	const path = "Packages/example-0.1.0-1.x86_64.rpm"
	if err := db.UpsertBlob(ctx, hash, "blobs/aa/11", 1234, "application/x-rpm"); err != nil {
		t.Fatal(err)
	}
	if err := db.CreateLocalFile(ctx, repo, path, hash); err != nil {
		t.Fatal(err)
	}

	h := NewObjectsHandler(db)
	router := chi.NewRouter()
	router.Route("/locals/{name}/objects", func(r chi.Router) {
		r.Get("/", h.LocalRoutes().ServeHTTP)
		r.Delete("/*", h.LocalRoutes().ServeHTTP)
	})

	// The uploaded package must appear in the listing with its blob size.
	req := httptest.NewRequest("GET", "/locals/"+repo+"/objects", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("list = %d, want 200", w.Code)
	}
	var got []models.Artifact
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d objects, want 1", len(got))
	}
	if got[0].Path != path || got[0].SizeBytes != 1234 || got[0].ContentHash != hash {
		t.Fatalf("unexpected object: %+v", got[0])
	}

	// Eviction removes it from local_files.
	del := httptest.NewRequest("DELETE", "/locals/"+repo+"/objects/"+path, nil)
	dw := httptest.NewRecorder()
	router.ServeHTTP(dw, del)
	if dw.Code != 204 {
		t.Fatalf("evict = %d, want 204", dw.Code)
	}
	if f, _ := db.GetLocalFile(ctx, repo, path); f != nil {
		t.Fatalf("file still present after evict: %+v", f)
	}
}
