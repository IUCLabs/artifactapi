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

// TestObjectsListPrefix verifies the remote objects listing passes ?prefix=
// through to the database filter.
func TestObjectsListPrefix(t *testing.T) {
	if testDSN == "" {
		t.Skip("Docker unavailable")
	}
	ctx := context.Background()
	db, err := database.New(testDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	const remote = "generic-objs-prefix"
	if err := db.CreateRemote(ctx, &models.Remote{
		Name: remote, PackageType: models.PackageGeneric, RepoType: models.RepoTypeRemote,
		BaseURL: "https://example.com", MutableTTL: 3600,
	}); err != nil {
		t.Fatal(err)
	}
	const hash = "sha256:bb22"
	if err := db.UpsertBlob(ctx, hash, "blobs/bb/22", 10, "text/plain"); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"a/one.txt", "b/two.txt"} {
		if err := db.UpsertArtifact(ctx, remote, p, hash, ""); err != nil {
			t.Fatal(err)
		}
	}

	router := chi.NewRouter()
	router.Mount("/remotes/{name}/objects", NewObjectsHandler(db).Routes())

	list := func(query string) []models.Artifact {
		t.Helper()
		w := httptest.NewRecorder()
		router.ServeHTTP(w, httptest.NewRequest("GET", "/remotes/"+remote+"/objects"+query, nil))
		if w.Code != 200 {
			t.Fatalf("list%s = %d, want 200", query, w.Code)
		}
		var got []models.Artifact
		if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return got
	}

	if got := list(""); len(got) != 2 {
		t.Fatalf("unfiltered listing returned %d objects, want 2", len(got))
	}
	if got := list("?prefix=b/"); len(got) != 1 || got[0].Path != "b/two.txt" {
		t.Fatalf("prefix=b/ listing = %+v, want only b/two.txt", got)
	}
}
