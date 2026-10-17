//go:build e2e

package e2e

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/IUCLabs/artifactapi/internal/testsupport"
)

// assertHeadMatchesGet checks HEAD returns GET's status, type and length with no body.
func assertHeadMatchesGet(t *testing.T, url string) {
	t.Helper()
	get, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	body, _ := io.ReadAll(get.Body)
	get.Body.Close()
	if get.StatusCode != http.StatusOK {
		t.Fatalf("GET %s: %d %s", url, get.StatusCode, body)
	}

	head, err := http.Head(url)
	if err != nil {
		t.Fatalf("HEAD: %v", err)
	}
	hb, _ := io.ReadAll(head.Body)
	head.Body.Close()
	if head.StatusCode != http.StatusOK {
		t.Fatalf("HEAD %s: got %d, want 200", url, head.StatusCode)
	}
	if len(hb) != 0 {
		t.Errorf("HEAD returned a %d-byte body", len(hb))
	}
	for _, h := range []string{"Content-Type", "Cache-Control", "Vary"} {
		if head.Header.Get(h) != get.Header.Get(h) {
			t.Errorf("HEAD %s %q, GET %q", h, head.Header.Get(h), get.Header.Get(h))
		}
	}
	if head.ContentLength != int64(len(body)) {
		t.Errorf("HEAD Content-Length %d, GET body %d bytes", head.ContentLength, len(body))
	}
}

func TestProxyHeadNuGetRewritten(t *testing.T) {
	var upstream *httptest.Server
	upstream = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"version": "3.0.0", "resources": []map[string]string{
			{"@id": upstream.URL + "/v3-flatcontainer/", "@type": "PackageBaseAddress/3.0.0"},
		}})
	}))
	defer upstream.Close()

	createRemote(t, mustFormat(`{"name": "head-nuget", "package_type": "nuget", "base_url": %q}`, upstream.URL+"/v3/index.json"))
	defer deleteRemote(t, "head-nuget")

	url := apiURL("/api/v1/remote/head-nuget/index.json")
	assertHeadMatchesGet(t, url)

	req, _ := http.NewRequest(http.MethodHead, url, nil)
	req.Host = "evil.example:"
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("HEAD with invalid Host: got %d, want 400", resp.StatusCode)
	}
}

func TestProxyHeadGitHubRPMRepodata(t *testing.T) {
	rpm := testsupport.MinimalRPM("demo", "1.2", "3", "x86_64")
	var gh *httptest.Server
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/acme/tools/releases", func(w http.ResponseWriter, r *http.Request) {
		if p := r.URL.Query().Get("page"); p != "" && p != "1" {
			w.Write([]byte("[]"))
			return
		}
		json.NewEncoder(w).Encode([]map[string]any{{"tag_name": "v1.2-3", "assets": []map[string]any{{
			"name":                 "demo-1.2-3.x86_64.rpm",
			"size":                 len(rpm),
			"browser_download_url": gh.URL + "/acme/tools/releases/download/v1.2-3/demo-1.2-3.x86_64.rpm",
		}}}})
	})
	mux.HandleFunc("/acme/tools/releases/download/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", strconv.Itoa(len(rpm)))
		w.Write(rpm)
	})
	gh = httptest.NewServer(mux)
	defer gh.Close()

	createRemote(t, mustFormat(`{"name": "head-ghrpm", "package_type": "github_rpm", "base_url": %q, "releases_remote": "github"}`, gh.URL+"/repos/acme/tools"))
	defer deleteRemote(t, "head-ghrpm")

	url := apiURL("/api/v1/remote/head-ghrpm/repodata/repomd.xml")
	deadline := time.Now().Add(30 * time.Second)
	for {
		if _, status := getBody(t, url); status == http.StatusOK || time.Now().After(deadline) {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	assertHeadMatchesGet(t, url)
}

func TestProxyHeadNpmUncachedPackument(t *testing.T) {
	const abbreviated = "application/vnd.npm.install-v1+json"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		doc := map[string]any{"name": "left-pad", "versions": map[string]any{"1.3.0": map[string]any{}}}
		if r.Header.Get("Accept") != abbreviated {
			doc["readme"] = strings.Repeat("full packument ", 200)
		}
		json.NewEncoder(w).Encode(doc)
	}))
	defer upstream.Close()

	createRemote(t, mustFormat(`{"name": "head-npm", "package_type": "npm", "base_url": %q}`, upstream.URL))
	defer deleteRemote(t, "head-npm")

	url := apiURL("/api/v1/remote/head-npm/left-pad")
	do := func(method string) (*http.Response, []byte) {
		req, _ := http.NewRequest(method, url, nil)
		req.Header.Set("Accept", abbreviated)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s: %v", method, err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp, b
	}
	head, _ := do(http.MethodHead)
	get, body := do(http.MethodGet)
	if head.StatusCode != http.StatusOK || get.StatusCode != http.StatusOK {
		t.Fatalf("HEAD %d, GET %d", head.StatusCode, get.StatusCode)
	}
	if head.ContentLength != int64(len(body)) {
		t.Errorf("uncached HEAD Content-Length %d, GET body %d bytes", head.ContentLength, len(body))
	}
}

func TestProxyHeadNonRewritingUsesUpstreamHead(t *testing.T) {
	methods := make(chan string, 4)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		methods <- r.Method
		w.Header().Set("Content-Type", "text/xml")
		w.Write([]byte("<repomd/>"))
	}))
	defer upstream.Close()

	createRemote(t, mustFormat(`{"name": "head-rpm", "package_type": "rpm", "base_url": %q}`, upstream.URL))
	defer deleteRemote(t, "head-rpm")

	resp, err := http.Head(apiURL("/api/v1/remote/head-rpm/repodata/repomd.xml"))
	if err != nil {
		t.Fatalf("HEAD: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || resp.Header.Get("X-Artifact-Source") != "remote" {
		t.Fatalf("HEAD %d source %q", resp.StatusCode, resp.Header.Get("X-Artifact-Source"))
	}
	close(methods)
	var seen []string
	for m := range methods {
		seen = append(seen, m)
	}
	if len(seen) != 1 || seen[0] != http.MethodHead {
		t.Errorf("upstream saw %v, want a single HEAD", seen)
	}
}
