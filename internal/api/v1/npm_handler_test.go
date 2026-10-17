package v1

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/IUCLabs/artifactapi/internal/cache"
	"github.com/IUCLabs/artifactapi/internal/database"
	"github.com/IUCLabs/artifactapi/internal/proxy"
	"github.com/IUCLabs/artifactapi/internal/storage"
	"github.com/IUCLabs/artifactapi/internal/testsupport"
	"github.com/IUCLabs/artifactapi/internal/virtual"
	"github.com/IUCLabs/artifactapi/pkg/models"
)

var (
	testDSN   string
	testMinio testsupport.MinioConn
	testRedis string
)

func TestMain(m *testing.M) {
	ctx := context.Background()
	dsn, termPG, err := testsupport.StartPostgres(ctx)
	if err != nil {
		os.Exit(m.Run())
	}
	conn, termMinio, err := testsupport.StartMinio(ctx)
	if err != nil {
		termPG()
		os.Exit(m.Run())
	}
	redisURL, termRedis, err := testsupport.StartRedis(ctx)
	if err != nil {
		termMinio()
		termPG()
		os.Exit(m.Run())
	}
	testDSN, testMinio, testRedis = dsn, conn, redisURL
	code := m.Run()
	termRedis()
	termMinio()
	termPG()
	os.Exit(code)
}

func npmTestServer(t *testing.T) (*ProxyHandler, *httptest.Server) {
	t.Helper()
	if testDSN == "" {
		t.Skip("Docker unavailable")
	}
	db, err := database.New(testDSN)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(db.Close)
	var store *storage.S3
	for i := 0; i < 20; i++ {
		if store, err = storage.NewS3(testMinio.Endpoint, testMinio.AccessKey, testMinio.SecretKey, "npm", false, ""); err == nil {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	if err != nil {
		t.Fatal(err)
	}
	redis, err := cache.NewRedis(testRedis)
	if err != nil {
		t.Fatal(err)
	}
	h := NewProxyHandler(proxy.NewEngine(db, redis, store), nil, db, store, nil)
	r := chi.NewRouter()
	r.Mount("/api/v1", h.Routes())
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	return h, srv
}

func npmRepo(t *testing.T, h *ProxyHandler, pt models.PackageType) string {
	t.Helper()
	name := fmt.Sprintf("npm-%s-%d", pt, time.Now().UnixNano())
	if err := h.db.CreateRemote(context.Background(), &models.Remote{Name: name, PackageType: pt, RepoType: models.RepoTypeLocal}); err != nil {
		t.Fatal(err)
	}
	return name
}

func npmDo(t *testing.T, method, url, body string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(method, url, strings.NewReader(body))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func packumentVersions(t *testing.T, url string) map[string]any {
	t.Helper()
	code, body := npmDo(t, http.MethodGet, url, "")
	if code != http.StatusOK {
		t.Fatalf("GET %s = %d: %s", url, code, body)
	}
	var doc struct {
		Versions map[string]any `json:"versions"`
	}
	if err := json.Unmarshal([]byte(body), &doc); err != nil {
		t.Fatal(err)
	}
	return doc.Versions
}

func packumentTags(t *testing.T, url string) map[string]string {
	t.Helper()
	_, body := npmDo(t, http.MethodGet, url, "")
	var doc struct {
		Tags map[string]string `json:"dist-tags"`
	}
	if err := json.Unmarshal([]byte(body), &doc); err != nil {
		t.Fatal(err)
	}
	return doc.Tags
}

func concurrentPuts(t *testing.T, url string, bodies ...string) []int {
	t.Helper()
	var wg sync.WaitGroup
	codes := make([]int, len(bodies))
	for i, b := range bodies {
		wg.Add(1)
		go func() {
			defer wg.Done()
			codes[i], _ = npmDo(t, http.MethodPut, url, b)
		}()
	}
	wg.Wait()
	return codes
}

func TestNPMPublishHandler(t *testing.T) {
	h, srv := npmTestServer(t)
	repo := npmRepo(t, h, models.PackageNPM)
	base := srv.URL + "/api/v1/local/" + repo + "/"

	t.Run("scoped name via %2f", func(t *testing.T) {
		body := publishBody(t, "@acme/widget", []byte("scoped tgz"), publishOpts{})
		if code, b := npmDo(t, http.MethodPut, base+"@acme%2fwidget", body); code != http.StatusCreated {
			t.Fatalf("publish = %d: %s", code, b)
		}
		v := packumentVersions(t, base+"@acme%2fwidget")
		dist := v["1.0.0"].(map[string]any)["dist"].(map[string]any)
		tarball := dist["tarball"].(string)
		if !strings.HasPrefix(tarball, base+"@acme/widget/-/widget-1.0.0.tgz") {
			t.Fatalf("tarball = %s", tarball)
		}
		if code, b := npmDo(t, http.MethodGet, tarball, ""); code != http.StatusOK || b != "scoped tgz" {
			t.Fatalf("tarball fetch = %d %q", code, b)
		}
	})

	t.Run("republish", func(t *testing.T) {
		if code, b := npmDo(t, http.MethodPut, base+"repub", publishBody(t, "repub", []byte("v1"), publishOpts{})); code != http.StatusCreated {
			t.Fatalf("first publish = %d: %s", code, b)
		}
		if code, _ := npmDo(t, http.MethodPut, base+"repub", publishBody(t, "repub", []byte("different"), publishOpts{})); code != http.StatusConflict {
			t.Errorf("different bytes = %d, want 409", code)
		}
		if code, b := npmDo(t, http.MethodPut, base+"repub", publishBody(t, "repub", []byte("v2"), publishOpts{version: "1.1.0"})); code != http.StatusCreated {
			t.Fatalf("publish 1.1.0 = %d: %s", code, b)
		}
		if code, _ := npmDo(t, http.MethodPut, base+"repub", publishBody(t, "repub", []byte("v1"), publishOpts{})); code != http.StatusConflict {
			t.Errorf("identical republish = %d, want 409", code)
		}
		if tags := packumentTags(t, base+"repub"); tags["latest"] != "1.1.0" {
			t.Errorf("latest = %q, want 1.1.0", tags["latest"])
		}
	})

	t.Run("rejected republish leaves tags", func(t *testing.T) {
		for _, v := range []string{"1.0.0", "1.1.0"} {
			if code, b := npmDo(t, http.MethodPut, base+"rej", publishBody(t, "rej", []byte("tgz "+v), publishOpts{version: v})); code != http.StatusCreated {
				t.Fatalf("publish %s = %d: %s", v, code, b)
			}
		}
		if code, _ := npmDo(t, http.MethodPut, base+"rej", publishBody(t, "rej", []byte("tgz 1.0.0"), publishOpts{shasum: "x"})); code != http.StatusConflict {
			t.Fatalf("re-PUT 1.0.0 = %d, want 409", code)
		}
		if tags := packumentTags(t, base+"rej"); tags["latest"] != "1.1.0" {
			t.Errorf("latest = %q, want 1.1.0", tags["latest"])
		}
	})

	t.Run("mid-publish window", func(t *testing.T) {
		if code, b := npmDo(t, http.MethodPut, base+"mid", publishBody(t, "mid", []byte("v1"), publishOpts{})); code != http.StatusCreated {
			t.Fatalf("publish = %d: %s", code, b)
		}
		ctx := context.Background()
		if _, err := h.npmStore(ctx, repo, "mid/-/mid-1.0.1.tgz", []byte("v2"), "application/octet-stream", false); err != nil {
			t.Fatal(err)
		}
		if tags := packumentTags(t, base+"mid"); tags["latest"] != "1.0.0" {
			t.Errorf("tarball-only 1.0.1: latest = %q", tags["latest"])
		}
		if _, err := h.npmStore(ctx, repo, "mid/-/dist-tags/latest", []byte("1.0.1"), "text/plain", true); err != nil {
			t.Fatal(err)
		}
		if tags := packumentTags(t, base+"mid"); tags["latest"] == "1.0.1" {
			t.Error("latest points at invisible 1.0.1")
		}
	})

	t.Run("concurrent identical", func(t *testing.T) {
		body := publishBody(t, "same", []byte("same tgz"), publishOpts{})
		codes := concurrentPuts(t, base+"same", body, body)
		if codes[0]+codes[1] != http.StatusCreated+http.StatusConflict || codes[0] == codes[1] {
			t.Fatalf("codes = %v, want one 201 and one 409", codes)
		}
		if tags := packumentTags(t, base+"same"); len(tags) != 1 || tags["latest"] != "1.0.0" {
			t.Errorf("tags = %v", tags)
		}
	})

	t.Run("concurrent different manifests", func(t *testing.T) {
		tgz := []byte("race tgz")
		codes := concurrentPuts(t, base+"race",
			publishBody(t, "race", tgz, publishOpts{shasum: "a", tags: map[string]string{"a": "1.0.0"}}),
			publishBody(t, "race", tgz, publishOpts{shasum: "b", tags: map[string]string{"b": "1.0.0"}}))
		won := map[int]string{}
		for i, c := range codes {
			won[c] = []string{"a", "b"}[i]
		}
		if len(won) != 2 || won[http.StatusCreated] == "" || won[http.StatusConflict] == "" {
			t.Fatalf("codes = %v, want one 201 and one 409", codes)
		}
		v := packumentVersions(t, base+"race")["1.0.0"].(map[string]any)
		if got := v["dist"].(map[string]any)["shasum"]; got != won[http.StatusCreated] {
			t.Errorf("manifest shasum = %v, winner %s", got, won[http.StatusCreated])
		}
		tags := packumentTags(t, base+"race")
		if len(tags) != 1 || tags[won[http.StatusCreated]] != "1.0.0" {
			t.Errorf("tags = %v, winner %s", tags, won[http.StatusCreated])
		}
	})

	t.Run("retry after tarball-only write", func(t *testing.T) {
		tgz := []byte("half published")
		if _, err := h.npmStore(context.Background(), repo, "retry/-/retry-1.0.0.tgz", tgz, "application/octet-stream", false); err != nil {
			t.Fatal(err)
		}
		if code, _ := npmDo(t, http.MethodGet, base+"retry", ""); code != http.StatusNotFound {
			t.Fatalf("tarball-only version visible: %d", code)
		}
		if code, _ := npmDo(t, http.MethodPut, base+"retry", publishBody(t, "retry", []byte("other bytes"), publishOpts{})); code != http.StatusConflict {
			t.Errorf("retry with different bytes = %d, want 409", code)
		}
		if code, b := npmDo(t, http.MethodPut, base+"retry", publishBody(t, "retry", tgz, publishOpts{})); code != http.StatusCreated {
			t.Fatalf("retry with identical bytes = %d: %s", code, b)
		}
		if _, ok := packumentVersions(t, base+"retry")["1.0.0"]; !ok {
			t.Error("retried version missing from packument")
		}
	})

	t.Run("concurrent versions", func(t *testing.T) {
		codes := concurrentPuts(t, base+"conc",
			publishBody(t, "conc", []byte("tgz 1.0.0"), publishOpts{version: "1.0.0"}),
			publishBody(t, "conc", []byte("tgz 2.0.0"), publishOpts{version: "2.0.0"}))
		if codes[0] != http.StatusCreated || codes[1] != http.StatusCreated {
			t.Fatalf("codes = %v", codes)
		}
		v := packumentVersions(t, base+"conc")
		if _, ok := v["1.0.0"]; !ok {
			t.Error("1.0.0 missing")
		}
		if _, ok := v["2.0.0"]; !ok {
			t.Error("2.0.0 missing")
		}
	})

	t.Run("body too large", func(t *testing.T) {
		old := npmMaxPublishBytes
		npmMaxPublishBytes = 64
		defer func() { npmMaxPublishBytes = old }()
		if code, _ := npmDo(t, http.MethodPut, base+"big", publishBody(t, "big", []byte("x"), publishOpts{})); code != http.StatusRequestEntityTooLarge {
			t.Errorf("oversized body = %d, want 413", code)
		}
	})

	t.Run("non-npm local", func(t *testing.T) {
		generic := npmRepo(t, h, models.PackageGeneric)
		if code, _ := npmDo(t, http.MethodPut, srv.URL+"/api/v1/local/"+generic+"/pkg", publishBody(t, "pkg", []byte("x"), publishOpts{})); code != http.StatusMethodNotAllowed {
			t.Errorf("publish to generic local = %d, want 405", code)
		}
	})
}

func TestNPMPackumentPerHost(t *testing.T) {
	h, srv := npmTestServer(t)
	h.virtualEngine = virtual.NewEngine(h.db, nil)
	h.virtualEngine.NPMLocal = h.LocalNPMPackument
	repo := npmRepo(t, h, models.PackageNPM)
	virt := "v" + repo
	if err := h.db.CreateVirtual(context.Background(), &models.Virtual{Name: virt, PackageType: models.PackageNPM, Members: []string{repo}}); err != nil {
		t.Fatal(err)
	}
	if code, b := npmDo(t, http.MethodPut, srv.URL+"/api/v1/local/"+repo+"/hostpkg", publishBody(t, "hostpkg", []byte("tgz"), publishOpts{})); code != http.StatusCreated {
		t.Fatalf("publish = %d: %s", code, b)
	}
	routes := h.Routes()
	get := func(path, host, proto string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodGet, path, nil)
		r.Host = host
		if proto != "" {
			r.Header.Set("X-Forwarded-Proto", proto)
		}
		w := httptest.NewRecorder()
		routes.ServeHTTP(w, r)
		return w
	}
	for _, path := range []string{"/local/" + repo + "/hostpkg", "/virtual/" + virt + "/hostpkg"} {
		for _, host := range []string{"x@evil.com", "evil.example:"} {
			if w := get(path, host, ""); w.Code != http.StatusBadRequest {
				t.Errorf("%s Host %q = %d, want 400", path, host, w.Code)
			}
		}
		w := get(path, "aa.example", "https")
		if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"tarball":"https://aa.example/api/v1/`) {
			t.Fatalf("%s = %d: %s", path, w.Code, w.Body)
		}
		if cc, vary := w.Header().Get("Cache-Control"), w.Header().Get("Vary"); cc != "no-store" || vary != "Host, X-Forwarded-Proto, Accept" {
			t.Errorf("%s Cache-Control %q Vary %q", path, cc, vary)
		}
	}
}
