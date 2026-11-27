package v1

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/IUCLabs/artifactapi/pkg/models"
)

func remoteRepo(t *testing.T, h *ProxyHandler, pt models.PackageType, baseURL, releases string) string {
	t.Helper()
	name := fmt.Sprintf("%s-%d", pt, time.Now().UnixNano())
	r := &models.Remote{Name: name, PackageType: pt, RepoType: models.RepoTypeRemote, BaseURL: baseURL, ReleasesRemote: releases}
	if err := h.db.CreateRemote(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	return name
}

func servedGet(t *testing.T, h *ProxyHandler, path string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, path, nil)
	r.Host = "aa.example"
	r.Header.Set("X-Forwarded-Proto", "https")
	w := httptest.NewRecorder()
	h.Routes().ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("GET %s = %d: %s", path, w.Code, w.Body)
	}
	if cc, vary := w.Header().Get("Cache-Control"), w.Header().Get("Vary"); cc != "no-store" || vary != "Host, X-Forwarded-Proto" {
		t.Errorf("Cache-Control %q Vary %q", cc, vary)
	}
	return w
}

func TestHelmIndexRewrittenAtServe(t *testing.T) {
	h, _ := npmTestServer(t)
	var upstream *httptest.Server
	upstream = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprintf(w, "apiVersion: v1\nentries:\n  demo:\n  - urls:\n    - %s/charts/demo-1.0.0.tgz\n    - https://other.example/demo-1.0.0.tgz\n", upstream.URL)
	}))
	t.Cleanup(upstream.Close)
	repo := remoteRepo(t, h, models.PackageHelm, upstream.URL, "")

	body := servedGet(t, h, "/remote/"+repo+"/index.yaml").Body.String()
	if want := "- https://aa.example/api/v1/remote/" + repo + "/charts/demo-1.0.0.tgz"; !strings.Contains(body, want) {
		t.Errorf("index.yaml missing %q:\n%s", want, body)
	}
	if !strings.Contains(body, "- https://other.example/demo-1.0.0.tgz") || strings.Contains(body, upstream.URL) {
		t.Errorf("only upstream-origin urls must be rewritten:\n%s", body)
	}
}

func TestTerraformDownloadRewrittenAtServe(t *testing.T) {
	h, _ := npmTestServer(t)
	releasesUp := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(releasesUp.Close)
	const gh = "https://github.com/integrations/terraform-provider-github/releases/download/v6.6.0/terraform-provider-github_6.6.0_linux_amd64.zip"
	registry := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		doc := map[string]string{"shasums_url": releasesUp.URL + "/terraform-provider-null/3.2.4/SHA256SUMS"}
		if strings.HasPrefix(r.URL.Path, "/v1/providers/integrations/") {
			doc["download_url"] = gh
		} else {
			doc["download_url"] = releasesUp.URL + "/terraform-provider-null/3.2.4/terraform-provider-null_3.2.4_linux_amd64.zip?x=1"
		}
		_ = json.NewEncoder(w).Encode(doc)
	}))
	t.Cleanup(registry.Close)
	releases := remoteRepo(t, h, models.PackageGeneric, releasesUp.URL, "")
	repo := remoteRepo(t, h, models.PackageTerraform, registry.URL, releases)

	var null map[string]string
	if err := json.Unmarshal(servedGet(t, h, "/remote/"+repo+"/hashicorp/null/3.2.4/download/linux/amd64").Body.Bytes(), &null); err != nil {
		t.Fatal(err)
	}
	prefix := "https://aa.example/api/v1/remote/" + releases + "/terraform-provider-null/3.2.4/"
	if null["download_url"] != prefix+"terraform-provider-null_3.2.4_linux_amd64.zip?x=1" || null["shasums_url"] != prefix+"SHA256SUMS" {
		t.Errorf("null download doc = %v", null)
	}

	var github map[string]string
	if err := json.Unmarshal(servedGet(t, h, "/remote/"+repo+"/integrations/github/6.6.0/download/linux/amd64").Body.Bytes(), &github); err != nil {
		t.Fatal(err)
	}
	if github["download_url"] != gh {
		t.Errorf("github download_url = %q, want untouched %q", github["download_url"], gh)
	}
}
