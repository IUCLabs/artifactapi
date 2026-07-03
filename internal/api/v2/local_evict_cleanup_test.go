package v2

import (
	"context"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/IUCLabs/artifactapi/internal/database"
	"github.com/IUCLabs/artifactapi/internal/provider"
	_ "github.com/IUCLabs/artifactapi/internal/provider/rpm" // register the rpm provider so its PostDeleteHook runs
	"github.com/IUCLabs/artifactapi/pkg/models"
)

// TestLocalEvictCleansRPMMetadata verifies that evicting an RPM from a local
// repo also removes the derived rpm_metadata row, so generated repodata stops
// listing the deleted package.
func TestLocalEvictCleansRPMMetadata(t *testing.T) {
	if testDSN == "" {
		t.Skip("Docker unavailable")
	}
	ctx := context.Background()
	db, err := database.New(testDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	const repo = "rpm-evict-cleanup"
	if err := db.CreateRemote(ctx, &models.Remote{Name: repo, PackageType: models.PackageRPM, RepoType: models.RepoTypeLocal}); err != nil {
		t.Fatal(err)
	}

	const hash = "sha256:bb22"
	const path = "Packages/example-0.1.0-1.x86_64.rpm"
	if err := db.UpsertBlob(ctx, hash, "blobs/bb/22", 2048, "application/x-rpm"); err != nil {
		t.Fatal(err)
	}
	if err := db.CreateLocalFile(ctx, repo, path, hash); err != nil {
		t.Fatal(err)
	}
	if err := db.InsertRPMMetadata(ctx, &provider.RPMMetadata{
		RepoName: repo, FilePath: path, ContentHash: hash,
		Name: "example", Version: "0.1.0", Release: "1", Arch: "x86_64",
		Requires: []provider.RPMDep{}, Provides: []provider.RPMDep{},
		Files: []provider.RPMFile{}, Changelogs: []provider.RPMChangelog{},
	}); err != nil {
		t.Fatal(err)
	}

	h := NewObjectsHandler(db)
	router := chi.NewRouter()
	router.Route("/locals/{name}/objects", func(r chi.Router) {
		r.Delete("/*", h.LocalRoutes().ServeHTTP)
	})

	del := httptest.NewRequest("DELETE", "/locals/"+repo+"/objects/"+path, nil)
	dw := httptest.NewRecorder()
	router.ServeHTTP(dw, del)
	if dw.Code != 204 {
		t.Fatalf("evict = %d, want 204", dw.Code)
	}

	if f, _ := db.GetLocalFile(ctx, repo, path); f != nil {
		t.Fatalf("local file still present after evict: %+v", f)
	}
	entries, err := db.ListRPMMetadataEntries(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("rpm_metadata still present after evict: %+v", entries)
	}
}
