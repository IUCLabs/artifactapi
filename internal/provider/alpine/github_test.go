package alpine

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/IUCLabs/artifactapi/internal/provider"
	"github.com/IUCLabs/artifactapi/internal/testsupport"
	"github.com/IUCLabs/artifactapi/pkg/models"
)

// fakeStore is an in-memory provider.RemoteMetadataStore + AlpineMetadata
// store/reader/deleter keyed by file_path, mirroring the (repo_name, file_path)
// uniqueness of the real alpine_metadata table.
type fakeStore struct {
	mu   sync.Mutex
	rows map[string]provider.AlpineMetadata
}

func newFakeStore() *fakeStore { return &fakeStore{rows: map[string]provider.AlpineMetadata{}} }

func (f *fakeStore) InsertAlpineMetadata(_ context.Context, m *provider.AlpineMetadata) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.rows[m.FilePath]; ok {
		return nil // ON CONFLICT DO NOTHING
	}
	f.rows[m.FilePath] = *m
	return nil
}

func (f *fakeStore) DeleteAlpineMetadata(_ context.Context, _, filePath string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.rows, filePath)
	return nil
}

func (f *fakeStore) ListAlpineMetadataEntries(ctx context.Context, _ string) ([]provider.AlpineMetadata, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]provider.AlpineMetadata, 0, len(f.rows))
	for _, m := range f.rows {
		out = append(out, m)
	}
	return out, nil
}

// The generic RemoteMetadataStore surface (rpm/deb) is unused by the alpine
// github provider but required to satisfy the interface passed to ServeRemote.
func (f *fakeStore) InsertRPMMetadata(context.Context, *provider.RPMMetadata) error { return nil }
func (f *fakeStore) DeleteRPMMetadata(context.Context, string, string) error        { return nil }
func (f *fakeStore) ListRPMMetadataEntries(context.Context, string) ([]provider.RPMMetadata, error) {
	return nil, nil
}
func (f *fakeStore) InsertDebMetadata(context.Context, *provider.DebMetadata) error { return nil }
func (f *fakeStore) DeleteDebMetadata(context.Context, string, string) error        { return nil }

var _ provider.RemoteMetadataStore = (*fakeStore)(nil)

// githubFixture serves the releases API and the .apk asset downloads (with Range
// support) for a set of packages. digest controls whether the asset carries a
// sha256 digest (change-detection path) or not.
type githubFixture struct {
	srv         *httptest.Server
	apkBytes    map[string][]byte
	rangeHit    map[string]int
	fullHit     map[string]int
	etag        string
	releasesHit int
	notModHit   int
	releaseAuth string
	assetAuth   string
	assetFail   map[string]int // asset filename -> downloads left to answer 500
	mu          sync.Mutex
}

func newGitHubFixture(t *testing.T, withDigest bool) *githubFixture {
	t.Helper()
	f := &githubFixture{
		apkBytes:  map[string][]byte{},
		rangeHit:  map[string]int{},
		fullHit:   map[string]int{},
		assetFail: map[string]int{},
	}
	f.apkBytes["demo-1.2.3-r0.apk"] = testsupport.MinimalApk("demo", "1.2.3-r0", "x86_64")

	mux := http.NewServeMux()
	mux.HandleFunc("/repos/acme/tools/releases", func(w http.ResponseWriter, r *http.Request) {
		page := r.URL.Query().Get("page")
		if page != "" && page != "1" {
			w.Write([]byte("[]"))
			return
		}
		f.mu.Lock()
		f.releasesHit++
		f.releaseAuth = r.Header.Get("Authorization")
		etag := f.etag
		if etag != "" && r.Header.Get("If-None-Match") == etag {
			f.notModHit++
			f.mu.Unlock()
			w.WriteHeader(http.StatusNotModified)
			return
		}
		f.mu.Unlock()
		if etag != "" {
			w.Header().Set("ETag", etag)
		}
		var assets []map[string]any
		for name := range f.apkBytes {
			a := map[string]any{
				"name":                 name,
				"size":                 len(f.apkBytes[name]),
				"browser_download_url": f.srv.URL + "/acme/tools/releases/download/v1.2.3/" + name,
			}
			if withDigest {
				sum := sha256.Sum256(f.apkBytes[name])
				a["digest"] = "sha256:" + hex.EncodeToString(sum[:])
			}
			assets = append(assets, a)
		}
		rel := []map[string]any{{"tag_name": "v1.2.3", "draft": false, "assets": assets}}
		json.NewEncoder(w).Encode(rel)
	})
	mux.HandleFunc("/acme/tools/releases/download/", func(w http.ResponseWriter, r *http.Request) {
		name := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
		body, ok := f.apkBytes[name]
		if !ok {
			http.Error(w, "not found", 404)
			return
		}
		rng := r.Header.Get("Range")
		f.mu.Lock()
		if f.assetFail[name] > 0 {
			f.assetFail[name]--
			f.mu.Unlock()
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		f.assetAuth = r.Header.Get("Authorization")
		if rng != "" {
			f.rangeHit[name]++
		} else {
			f.fullHit[name]++
		}
		f.mu.Unlock()

		if rng == "" {
			w.WriteHeader(200)
			w.Write(body)
			return
		}
		var end int
		fmt.Sscanf(rng, "bytes=0-%d", &end)
		if end >= len(body)-1 {
			end = len(body) - 1
		}
		w.Header().Set("Content-Range", fmt.Sprintf("bytes 0-%d/%d", end, len(body)))
		w.Header().Set("Content-Length", strconv.Itoa(end+1))
		w.WriteHeader(http.StatusPartialContent)
		w.Write(body[:end+1])
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *githubFixture) remote() models.Remote {
	return models.Remote{
		Name:           "acme-apk",
		PackageType:    models.PackageGitHubAlpine,
		BaseURL:        f.srv.URL + "/repos/acme/tools",
		ReleasesRemote: "github",
		MutableTTL:     3600,
	}
}

func newTestProvider() *GitHubProvider {
	p := newGitHubProvider()
	p.headerInitial = 32 // force the ranged-fetch retry loop against the tiny fixture
	p.headerMax = 1 << 20
	return p
}

const demoPath = "acme/tools/releases/download/v1.2.3/demo-1.2.3-r0.apk"

func TestGitHubScanDerivesPkginfoFromPrefix(t *testing.T) {
	fx := newGitHubFixture(t, true)
	p := newTestProvider()
	store := newFakeStore()

	if err := p.scan(context.Background(), fx.remote(), store); err != nil {
		t.Fatalf("scan: %v", err)
	}

	metas, _ := store.ListAlpineMetadataEntries(context.Background(), "acme-apk")
	if len(metas) != 1 {
		t.Fatalf("want 1 metadata row, got %d", len(metas))
	}
	m := metas[0]
	if m.Name != "demo" || m.Version != "1.2.3-r0" || m.Arch != "x86_64" {
		t.Fatalf("bad .PKGINFO fields: %+v", m)
	}
	if m.FilePath != demoPath {
		t.Fatalf("FilePath = %q, want %q", m.FilePath, demoPath)
	}
	if int(m.DownloadSize) != len(fx.apkBytes["demo-1.2.3-r0.apk"]) {
		t.Fatalf("DownloadSize = %d, want %d", m.DownloadSize, len(fx.apkBytes["demo-1.2.3-r0.apk"]))
	}
	if !strings.HasPrefix(m.Checksum, "Q1") {
		t.Fatalf("Checksum not a Q1 pull checksum: %q", m.Checksum)
	}
	// The C: checksum must equal Q1 over the raw control gzip stream, matching the
	// local-upload parser applied to the same bytes.
	want, err := parseApk(fx.apkBytes["demo-1.2.3-r0.apk"])
	if err != nil {
		t.Fatalf("reference parseApk: %v", err)
	}
	if m.Checksum != want.Checksum {
		t.Fatalf("Checksum = %q, want %q (Q1 of control stream)", m.Checksum, want.Checksum)
	}
	if fx.fullHit["demo-1.2.3-r0.apk"] != 0 {
		t.Fatalf("expected no full download, got %d", fx.fullHit["demo-1.2.3-r0.apk"])
	}
	if fx.rangeHit["demo-1.2.3-r0.apk"] == 0 {
		t.Fatalf("expected ranged .PKGINFO fetch")
	}
}

func TestGitHubServeRemoteIndexAndRedirect(t *testing.T) {
	fx := newGitHubFixture(t, true)
	p := newTestProvider()
	store := newFakeStore()
	remote := fx.remote()
	const proxyBase = "https://artifactapi.example"

	// The per-arch index is served and triggers the initial scan.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/remote/acme-apk/x86_64/APKINDEX.tar.gz", nil)
	if !p.ServeRemote(rec, req, remote, "x86_64/APKINDEX.tar.gz", proxyBase, store) {
		t.Fatal("ServeRemote did not handle APKINDEX")
	}
	if rec.Code != 200 {
		t.Fatalf("APKINDEX bad: code=%d body=%s", rec.Code, rec.Body.String())
	}
	idx := readAPKIndex(t, rec.Body.Bytes())
	if !strings.Contains(idx, "P:demo") || !strings.Contains(idx, "A:x86_64") {
		t.Fatalf("APKINDEX missing package record: %s", idx)
	}
	if !strings.Contains(idx, "C:Q1") {
		t.Fatalf("APKINDEX missing pull checksum: %s", idx)
	}

	// A different arch yields an empty (but valid) index.
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/x", nil)
	if !p.ServeRemote(rec, req, remote, "aarch64/APKINDEX.tar.gz", proxyBase, store) {
		t.Fatal("ServeRemote did not handle aarch64 APKINDEX")
	}
	if rec.Code != 200 {
		t.Fatalf("empty-arch index bad: %d", rec.Code)
	}
	if got := readAPKIndex(t, rec.Body.Bytes()); strings.Contains(got, "P:demo") {
		t.Fatalf("aarch64 index should not carry the x86_64 package: %s", got)
	}

	// An .apk request arrives in apk's reconstructed shape
	// "<arch>/<name>-<version>.apk" (APKINDEX carries no filename), NOT as the
	// github-relative FilePath. ServeRemote must resolve it back to the stored
	// FilePath before redirecting to the backend releases_remote.
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/api/v1/remote/acme-apk/x86_64/demo-1.2.3-r0.apk", nil)
	if !p.ServeRemote(rec, req, remote, "x86_64/demo-1.2.3-r0.apk", proxyBase, store) {
		t.Fatal("ServeRemote did not handle .apk")
	}
	if rec.Code != http.StatusFound {
		t.Fatalf("want 302, got %d", rec.Code)
	}
	wantLoc := proxyBase + "/api/v1/remote/github/" + demoPath
	if got := rec.Header().Get("Location"); got != wantLoc {
		t.Fatalf("Location = %q, want %q (must be the stored FilePath, not the inbound path)", got, wantLoc)
	}
}

// An apk download whose reconstructed "<arch>/<name>-<version>.apk" matches no
// cached row must 404, never redirect to a bad path.
func TestGitHubServeRemoteApkRedirectNotFound(t *testing.T) {
	fx := newGitHubFixture(t, true)
	p := newTestProvider()
	store := newFakeStore()
	remote := fx.remote()

	// Warm the cache so the store is populated but lacks the requested package.
	if err := p.scan(context.Background(), remote, store); err != nil {
		t.Fatalf("warm scan: %v", err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/remote/acme-apk/x86_64/nope-9.9.9.apk", nil)
	if !p.ServeRemote(rec, req, remote, "x86_64/nope-9.9.9.apk", "https://x", store) {
		t.Fatal("ServeRemote did not handle .apk")
	}
	if rec.Code != http.StatusNotFound {
		t.Fatalf("want 404 for unknown package, got %d (Location=%q)", rec.Code, rec.Header().Get("Location"))
	}
}

// apk requests the index at "./<arch>/APKINDEX.tar.gz"; ServeRemote must collapse
// the dot-segment and synthesize the same index as the un-prefixed request.
func TestGitHubServeRemoteApkDotSegment(t *testing.T) {
	fx := newGitHubFixture(t, true)
	p := newTestProvider()
	store := newFakeStore()
	remote := fx.remote()
	const proxyBase = "https://artifactapi.example"

	serve := func(path string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/api/v1/remote/acme-apk/"+path, nil)
		if !p.ServeRemote(rec, req, remote, path, proxyBase, store) {
			t.Fatalf("ServeRemote did not handle %q", path)
		}
		return rec
	}

	plain, dotted := serve("x86_64/APKINDEX.tar.gz"), serve("./x86_64/APKINDEX.tar.gz")
	if plain.Code != 200 || dotted.Code != 200 {
		t.Fatalf("index: plain=%d dotted=%d, want 200/200", plain.Code, dotted.Code)
	}
	if !bytes.Equal(plain.Body.Bytes(), dotted.Body.Bytes()) {
		t.Error("./<arch>/APKINDEX.tar.gz body differs from the un-prefixed body")
	}
}

func TestGitHubServeRemoteRejectsNonPerArchIndex(t *testing.T) {
	fx := newGitHubFixture(t, true)
	p := newTestProvider()
	store := newFakeStore()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	if !p.ServeRemote(rec, req, fx.remote(), "APKINDEX.tar.gz", "https://x", store) {
		t.Fatal("expected handled")
	}
	if rec.Code != http.StatusNotFound {
		t.Fatalf("bare APKINDEX must 404 (per-arch required), got %d", rec.Code)
	}
}

// A canceled inbound request must still serve the warm cache (detached context),
// not turn the metadata read into a 500.
func TestGitHubServeRemoteCanceledRequestServesCache(t *testing.T) {
	fx := newGitHubFixture(t, true)
	p := newTestProvider()
	store := newFakeStore()
	remote := fx.remote()

	if err := p.scan(context.Background(), remote, store); err != nil {
		t.Fatalf("warm scan: %v", err)
	}
	p.mu.Lock()
	p.lastScan[remote.Name] = time.Now()
	p.mu.Unlock()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/remote/acme-apk/x86_64/APKINDEX.tar.gz", nil).WithContext(ctx)

	if !p.ServeRemote(rec, req, remote, "x86_64/APKINDEX.tar.gz", "https://x", store) {
		t.Fatal("ServeRemote did not handle APKINDEX")
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("canceled request must serve cache, not error; got code=%d body=%s", rec.Code, rec.Body.String())
	}
	if got := readAPKIndex(t, rec.Body.Bytes()); !strings.Contains(got, "P:demo") {
		t.Fatalf("expected index served from cache, got %s", got)
	}
}

func TestGitHubServeRemoteRedirectRequiresReleasesRemote(t *testing.T) {
	fx := newGitHubFixture(t, true)
	p := newTestProvider()
	store := newFakeStore()
	remote := fx.remote()
	remote.ReleasesRemote = ""

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	if !p.ServeRemote(rec, req, remote, demoPath, "https://x", store) {
		t.Fatal("expected handled")
	}
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("want 500 when releases_remote unset, got %d", rec.Code)
	}
}

func TestGitHubScanPrunesRemovedAssets(t *testing.T) {
	fx := newGitHubFixture(t, true)
	p := newTestProvider()
	store := newFakeStore()

	if err := p.scan(context.Background(), fx.remote(), store); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if rows, _ := store.ListAlpineMetadataEntries(context.Background(), "acme-apk"); len(rows) != 1 {
		t.Fatalf("want 1 row after first scan, got %d", len(rows))
	}

	delete(fx.apkBytes, "demo-1.2.3-r0.apk")
	if err := p.scan(context.Background(), fx.remote(), store); err != nil {
		t.Fatalf("rescan: %v", err)
	}
	if rows, _ := store.ListAlpineMetadataEntries(context.Background(), "acme-apk"); len(rows) != 0 {
		t.Fatalf("want 0 rows after prune, got %d", len(rows))
	}
}

func TestGitHubAssetPatternFilter(t *testing.T) {
	fx := newGitHubFixture(t, true)
	fx.apkBytes["other-9-r0.apk"] = testsupport.MinimalApk("other", "9-r0", "aarch64")
	p := newTestProvider()
	store := newFakeStore()
	remote := fx.remote()
	remote.Patterns = []string{`^demo-.*\.apk$`}

	if err := p.scan(context.Background(), remote, store); err != nil {
		t.Fatalf("scan: %v", err)
	}
	rows, _ := store.ListAlpineMetadataEntries(context.Background(), "acme-apk")
	if len(rows) != 1 || rows[0].Name != "demo" {
		t.Fatalf("pattern filter failed, rows=%+v", rows)
	}
}

// Multi-arch: each asset's index record lands under its own arch bucket.
func TestGitHubServeRemotePerArchGrouping(t *testing.T) {
	fx := newGitHubFixture(t, true)
	fx.apkBytes["demo-1.2.3-r0-aarch64.apk"] = testsupport.MinimalApk("demo", "1.2.3-r0", "aarch64")
	p := newTestProvider()
	store := newFakeStore()
	remote := fx.remote()
	const proxyBase = "https://x"

	if err := p.scan(context.Background(), remote, store); err != nil {
		t.Fatalf("scan: %v", err)
	}

	serve := func(arch string) string {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/x", nil)
		if !p.ServeRemote(rec, req, remote, arch+"/APKINDEX.tar.gz", proxyBase, store) {
			t.Fatalf("ServeRemote did not handle %s", arch)
		}
		return readAPKIndex(t, rec.Body.Bytes())
	}

	x86 := serve("x86_64")
	if !strings.Contains(x86, "A:x86_64") || strings.Contains(x86, "A:aarch64") {
		t.Fatalf("x86_64 index leaked another arch: %s", x86)
	}
	arm := serve("aarch64")
	if !strings.Contains(arm, "A:aarch64") || strings.Contains(arm, "A:x86_64") {
		t.Fatalf("aarch64 index leaked another arch: %s", arm)
	}
}

func readAPKIndex(t *testing.T, gzBytes []byte) string {
	t.Helper()
	gz, err := gzip.NewReader(bytes.NewReader(gzBytes))
	if err != nil {
		t.Fatalf("gzip: %v", err)
	}
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err != nil {
			t.Fatal("APKINDEX member missing from tar.gz")
		}
		if strings.TrimPrefix(hdr.Name, "./") == "APKINDEX" {
			body, err := io.ReadAll(tr)
			if err != nil {
				t.Fatalf("read APKINDEX: %v", err)
			}
			return string(body)
		}
	}
}

func TestGitHubDeriveAssetMissingPkgnameIsInvalid(t *testing.T) {
	fx := newGitHubFixture(t, true)
	fx.apkBytes["nameless-1-r0.apk"] = testsupport.MinimalApk("", "1-r0", "x86_64")
	asset := ghAsset{BrowserDownloadURL: fx.srv.URL + "/acme/tools/releases/download/v1.2.3/nameless-1-r0.apk"}
	_, err := newTestProvider().deriveAsset(context.Background(), fx.remote(), asset, "fp")
	if !errors.Is(err, provider.ErrInvalidPackage) {
		t.Fatalf("want ErrInvalidPackage, got %v", err)
	}
}
