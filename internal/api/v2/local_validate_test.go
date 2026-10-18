package v2

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/IUCLabs/artifactapi/internal/database"
	_ "github.com/IUCLabs/artifactapi/internal/provider/goproxy"
	"github.com/IUCLabs/artifactapi/pkg/models"
)

// TestLocalUploadContentRejected verifies a ContentValidator provider rejects
// bad bytes with 400 before anything is stored.
func TestLocalUploadContentRejected(t *testing.T) {
	if testDSN == "" {
		t.Skip("Docker unavailable")
	}
	ctx := context.Background()
	db, err := database.New(testDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	const repo = "go-local-validate"
	if err := db.CreateRemote(ctx, &models.Remote{Name: repo, PackageType: models.PackageGoProxy, RepoType: models.RepoTypeLocal}); err != nil {
		t.Fatal(err)
	}

	router := chi.NewRouter()
	router.Route("/remotes/{name}/files", func(r chi.Router) {
		r.Put("/*", NewLocalHandler(db, nil).Routes().ServeHTTP)
	})

	cases := map[string]string{
		"myorg.example.net/hello/@v/v0.1.0.zip":  "not a zip",
		"myorg.example.net/hello/@v/v0.1.0.mod":  "module myorg.example.net/other\n",
		"myorg.example.net/hello/@v/v0.1.0.info": `{"Version":"v9.9.9"}`,
		"myorg.example.net/Hello/@v/v0.1.0.mod":  "module myorg.example.net/Hello\n",
	}
	for path, body := range cases {
		w := httptest.NewRecorder()
		router.ServeHTTP(w, httptest.NewRequest("PUT", "/remotes/"+repo+"/files/"+path, strings.NewReader(body)))
		if w.Code != 400 {
			t.Errorf("%s: status %d, want 400 (%s)", path, w.Code, w.Body.String())
		}
		if f, _ := db.GetLocalFile(ctx, repo, path); f != nil {
			t.Errorf("%s: rejected upload was recorded", path)
		}
	}
}
