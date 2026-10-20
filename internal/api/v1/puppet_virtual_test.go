package v1

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/IUCLabs/artifactapi/internal/provider"
	"github.com/IUCLabs/artifactapi/internal/virtual"
	"github.com/IUCLabs/artifactapi/pkg/models"
)

func TestPuppetVirtualOverLocals(t *testing.T) {
	h, srv := npmTestServer(t)
	h.virtualEngine = virtual.NewEngine(h.db, nil)
	a, b := npmRepo(t, h, models.PackagePuppet), npmRepo(t, h, models.PackagePuppet)
	for _, p := range []struct{ repo, version string }{{a, "0.1.0"}, {b, "0.1.0"}, {b, "0.2.0"}} {
		file := "v3/files/myorg-hello-" + p.version + ".tar.gz"
		meta := &provider.PuppetMetadata{RepoName: p.repo, FilePath: file, ContentHash: "sha256:00", Slug: "myorg-hello", Version: p.version, MetadataJSON: `{"name":"myorg-hello","version":"` + p.version + `"}`}
		if err := h.db.InsertPuppetMetadata(context.Background(), meta); err != nil {
			t.Fatal(err)
		}
	}
	virt := "v" + a
	if err := h.db.CreateVirtual(context.Background(), &models.Virtual{Name: virt, PackageType: models.PackagePuppet, Members: []string{a, b}}); err != nil {
		t.Fatal(err)
	}
	base := srv.URL + "/api/v1/virtual/" + virt

	var page struct {
		Pagination struct {
			Total int
			Next  *string
		}
		Results []struct {
			Version string
			FileURI string `json:"file_uri"`
		}
	}
	getJSON(t, base+"/v3/releases?module=myorg/hello&limit=1", &page)
	if page.Pagination.Total != 2 || len(page.Results) != 1 || page.Results[0].Version != "0.2.0" || page.Pagination.Next == nil {
		t.Fatalf("page 1 = %+v", page)
	}
	if *page.Pagination.Next != "/v3/releases?limit=1&module=myorg-hello&offset=1" || page.Results[0].FileURI != "/v3/files/myorg-hello-0.2.0.tar.gz" {
		t.Fatalf("links not relative to the forge root: next %q file_uri %q", *page.Pagination.Next, page.Results[0].FileURI)
	}
	getJSON(t, base+*page.Pagination.Next, &page)
	if len(page.Results) != 1 || page.Results[0].Version != "0.1.0" || page.Pagination.Next != nil {
		t.Fatalf("page 2 = %+v", page)
	}

	var mod struct {
		CurrentRelease struct{ Version string } `json:"current_release"`
		Releases       []struct{ Version string }
	}
	getJSON(t, base+"/v3/modules/myorg-hello", &mod)
	if mod.CurrentRelease.Version != "0.2.0" || len(mod.Releases) != 2 {
		t.Fatalf("module = %+v", mod)
	}

	noRedirect := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	for p, want := range map[string]string{
		"/v3/files/myorg-hello-0.1.0.tar.gz": "/api/v1/local/" + a + "/v3/files/myorg-hello-0.1.0.tar.gz",
		"/v3/releases/myorg-hello-0.2.0":     "/api/v1/local/" + b + "/v3/releases/myorg-hello-0.2.0",
	} {
		resp, err := noRedirect.Get(base + p)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusFound || resp.Header.Get("Location") != srv.URL+want || resp.Header.Get("Cache-Control") != "no-store" {
			t.Errorf("%s = %d %q %q, want 302 to %s", p, resp.StatusCode, resp.Header.Get("Location"), resp.Header.Get("Cache-Control"), want)
		}
	}

	getJSON(t, base+"/v3/releases?module=myorg-absent", &page)
	if page.Pagination.Total != 0 || len(page.Results) != 0 {
		t.Fatalf("absent module = %+v", page)
	}
	for p, want := range map[string]int{
		"/v3/releases":                       http.StatusBadRequest,
		"/v3/modules/myorg-absent":           http.StatusNotFound,
		"/v3/files/myorg-hello-0.9.0.tar.gz": http.StatusNotFound,
	} {
		if code, body := npmDo(t, http.MethodGet, base+p, ""); code != want || !strings.Contains(body, `"errors"`) {
			t.Errorf("%s = %d %s, want %d", p, code, body, want)
		}
	}
}
