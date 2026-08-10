package rpm

import (
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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

// fakeStore is an in-memory provider.RemoteMetadataStore keyed by file_path,
// mirroring the (repo_name, file_path) uniqueness of the real table.
type fakeStore struct {
	mu   sync.Mutex
	rows map[string]provider.RPMMetadata
}

func newFakeStore() *fakeStore { return &fakeStore{rows: map[string]provider.RPMMetadata{}} }

func (f *fakeStore) InsertRPMMetadata(_ context.Context, m *provider.RPMMetadata) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.rows[m.FilePath]; ok {
		return nil // ON CONFLICT DO NOTHING
	}
	f.rows[m.FilePath] = *m
	return nil
}

func (f *fakeStore) DeleteRPMMetadata(_ context.Context, _, filePath string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.rows, filePath)
	return nil
}

func (f *fakeStore) ListRPMMetadataEntries(ctx context.Context, _ string) ([]provider.RPMMetadata, error) {
	// Mirror pgx: a canceled/expired context fails the read. This is what
	// poisons the repodata response if the read runs on the inbound request.
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]provider.RPMMetadata, 0, len(f.rows))
	for _, m := range f.rows {
		out = append(out, m)
	}
	return out, nil
}

// githubFixture serves the releases API and the .rpm asset downloads (with
// Range support) for a set of packages. digest controls whether the asset
// carries a sha256 digest (no-download path) or not (compute path).
type githubFixture struct {
	srv         *httptest.Server
	rpmBytes    map[string][]byte // asset filename -> bytes
	rangeHit    map[string]int    // asset filename -> number of ranged GETs
	fullHit     map[string]int    // asset filename -> number of full GETs
	etag        string            // when set, served as ETag; matching If-None-Match yields 304
	releasesHit int               // total releases-list requests (200 + 304)
	notModHit   int               // releases-list requests answered 304
	mu          sync.Mutex
}

func newGitHubFixture(t *testing.T, withDigest bool) *githubFixture {
	t.Helper()
	f := &githubFixture{
		rpmBytes: map[string][]byte{},
		rangeHit: map[string]int{},
		fullHit:  map[string]int{},
	}
	f.rpmBytes["demo-1.2-3.x86_64.rpm"] = testsupport.MinimalRPM("demo", "1.2", "3", "x86_64")

	mux := http.NewServeMux()
	mux.HandleFunc("/repos/acme/tools/releases", func(w http.ResponseWriter, r *http.Request) {
		page := r.URL.Query().Get("page")
		if page != "" && page != "1" {
			w.Write([]byte("[]"))
			return
		}
		f.mu.Lock()
		f.releasesHit++
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
		for name := range f.rpmBytes {
			a := map[string]any{
				"name":                 name,
				"size":                 len(f.rpmBytes[name]),
				"browser_download_url": f.srv.URL + "/acme/tools/releases/download/v1.2-3/" + name,
			}
			if withDigest {
				sum := sha256.Sum256(f.rpmBytes[name])
				a["digest"] = "sha256:" + hex.EncodeToString(sum[:])
			}
			assets = append(assets, a)
		}
		rel := []map[string]any{{"tag_name": "v1.2-3", "draft": false, "assets": assets}}
		json.NewEncoder(w).Encode(rel)
	})
	mux.HandleFunc("/acme/tools/releases/download/", func(w http.ResponseWriter, r *http.Request) {
		name := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
		body, ok := f.rpmBytes[name]
		if !ok {
			http.Error(w, "not found", 404)
			return
		}
		rng := r.Header.Get("Range")
		f.mu.Lock()
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
		// Parse "bytes=0-N".
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
		Name:           "acme-rpm",
		PackageType:    models.PackageGitHubRPM,
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

func TestGitHubScanDerivesMetadataFromHeaderAndDigest(t *testing.T) {
	fx := newGitHubFixture(t, true)
	p := newTestProvider()
	store := newFakeStore()

	if err := p.scan(context.Background(), fx.remote(), store); err != nil {
		t.Fatalf("scan: %v", err)
	}

	metas, _ := store.ListRPMMetadataEntries(context.Background(), "acme-rpm")
	if len(metas) != 1 {
		t.Fatalf("want 1 metadata row, got %d", len(metas))
	}
	m := metas[0]
	if m.Name != "demo" || m.Version != "1.2" || m.Release != "3" || m.Arch != "x86_64" {
		t.Fatalf("bad NEVRA: %+v", m)
	}
	// location href / redirect key must be the github-relative download path.
	wantPath := "acme/tools/releases/download/v1.2-3/demo-1.2-3.x86_64.rpm"
	if m.FilePath != wantPath {
		t.Fatalf("FilePath = %q, want %q", m.FilePath, wantPath)
	}
	if int(m.RPMSize) != len(fx.rpmBytes["demo-1.2-3.x86_64.rpm"]) {
		t.Fatalf("RPMSize = %d, want %d", m.RPMSize, len(fx.rpmBytes["demo-1.2-3.x86_64.rpm"]))
	}
	// Digest present => checksum from digest, no full download.
	sum := sha256.Sum256(fx.rpmBytes["demo-1.2-3.x86_64.rpm"])
	if m.ContentHash != "sha256:"+hex.EncodeToString(sum[:]) {
		t.Fatalf("ContentHash = %q, want digest", m.ContentHash)
	}
	if fx.fullHit["demo-1.2-3.x86_64.rpm"] != 0 {
		t.Fatalf("expected no full download when digest present, got %d", fx.fullHit["demo-1.2-3.x86_64.rpm"])
	}
	if fx.rangeHit["demo-1.2-3.x86_64.rpm"] == 0 {
		t.Fatalf("expected ranged header fetch")
	}
}

func TestGitHubChecksumComputedWhenDigestAbsent(t *testing.T) {
	fx := newGitHubFixture(t, false)
	p := newTestProvider()
	store := newFakeStore()

	if err := p.scan(context.Background(), fx.remote(), store); err != nil {
		t.Fatalf("scan: %v", err)
	}
	metas, _ := store.ListRPMMetadataEntries(context.Background(), "acme-rpm")
	if len(metas) != 1 {
		t.Fatalf("want 1 row, got %d", len(metas))
	}
	sum := sha256.Sum256(fx.rpmBytes["demo-1.2-3.x86_64.rpm"])
	if metas[0].ContentHash != "sha256:"+hex.EncodeToString(sum[:]) {
		t.Fatalf("computed checksum mismatch: %q", metas[0].ContentHash)
	}
	if fx.fullHit["demo-1.2-3.x86_64.rpm"] == 0 {
		t.Fatalf("expected a full download to compute sha256 when digest absent")
	}
}

func TestGitHubServeRemoteRepodataAndRedirect(t *testing.T) {
	fx := newGitHubFixture(t, true)
	p := newTestProvider()
	store := newFakeStore()
	remote := fx.remote()
	const proxyBase = "https://artifactapi.example"

	// repomd.xml is served and triggers the initial scan.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/remote/acme-rpm/repodata/repomd.xml", nil)
	if !p.ServeRemote(rec, req, remote, "repodata/repomd.xml", proxyBase, store) {
		t.Fatal("ServeRemote did not handle repomd.xml")
	}
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "<repomd") {
		t.Fatalf("repomd bad: code=%d body=%s", rec.Code, rec.Body.String())
	}

	// primary.xml.gz must carry the package with a location href that is the
	// github-relative download path (so it resolves back to this remote and
	// redirects to the backend).
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/x", nil)
	if !p.ServeRemote(rec, req, remote, "repodata/abc-primary.xml.gz", proxyBase, store) {
		t.Fatal("ServeRemote did not handle primary")
	}
	gz, err := gzip.NewReader(rec.Body)
	if err != nil {
		t.Fatalf("gzip: %v", err)
	}
	xmlBytes, _ := io.ReadAll(gz)
	primary := string(xmlBytes)
	if !strings.Contains(primary, `<name>demo</name>`) {
		t.Fatalf("primary missing package: %s", primary)
	}
	if !strings.Contains(primary, `<location href="acme/tools/releases/download/v1.2-3/demo-1.2-3.x86_64.rpm"/>`) {
		t.Fatalf("primary missing/incorrect location href: %s", primary)
	}

	// A .rpm request redirects to the backend releases_remote.
	rec = httptest.NewRecorder()
	pkgPath := "acme/tools/releases/download/v1.2-3/demo-1.2-3.x86_64.rpm"
	req = httptest.NewRequest(http.MethodGet, "/api/v1/remote/acme-rpm/"+pkgPath, nil)
	if !p.ServeRemote(rec, req, remote, pkgPath, proxyBase, store) {
		t.Fatal("ServeRemote did not handle .rpm")
	}
	if rec.Code != http.StatusFound {
		t.Fatalf("want 302, got %d", rec.Code)
	}
	wantLoc := proxyBase + "/api/v1/remote/github/" + pkgPath
	if got := rec.Header().Get("Location"); got != wantLoc {
		t.Fatalf("Location = %q, want %q", got, wantLoc)
	}
}

// TestGitHubServeRemoteCanceledRequestServesCache reproduces the cold-makecache
// 500: when the inbound request context is already canceled (dnf timed out and
// disconnected), the repodata read must not be run on that context and turned
// into a 500. With the cache already warm, the handler serves it as 200.
// Before the fix the read used r.Context() and returned 500; after the fix it
// runs on a detached context and serves the cached repomd.
func TestGitHubServeRemoteCanceledRequestServesCache(t *testing.T) {
	fx := newGitHubFixture(t, true)
	p := newTestProvider()
	store := newFakeStore()
	remote := fx.remote()

	// Warm the cache and mark the scan fresh so ServeRemote does not re-derive.
	if err := p.scan(context.Background(), remote, store); err != nil {
		t.Fatalf("warm scan: %v", err)
	}
	p.mu.Lock()
	p.lastScan[remote.Name] = time.Now()
	p.mu.Unlock()

	// Inbound request whose context is already canceled (client went away).
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/remote/acme-rpm/repodata/repomd.xml", nil).WithContext(ctx)

	if !p.ServeRemote(rec, req, remote, "repodata/repomd.xml", "https://x", store) {
		t.Fatal("ServeRemote did not handle repomd.xml")
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("canceled request must serve cache, not error; got code=%d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "<repomd") {
		t.Fatalf("expected repomd served from cache, got %s", rec.Body.String())
	}
}

func TestGitHubServeRemoteRedirectRequiresReleasesRemote(t *testing.T) {
	fx := newGitHubFixture(t, true)
	p := newTestProvider()
	store := newFakeStore()
	remote := fx.remote()
	remote.ReleasesRemote = ""

	rec := httptest.NewRecorder()
	pkgPath := "acme/tools/releases/download/v1.2-3/demo-1.2-3.x86_64.rpm"
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	if !p.ServeRemote(rec, req, remote, pkgPath, "https://x", store) {
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
	if rows, _ := store.ListRPMMetadataEntries(context.Background(), "acme-rpm"); len(rows) != 1 {
		t.Fatalf("want 1 row after first scan, got %d", len(rows))
	}

	// Remove the asset upstream; a rescan must prune the stale metadata row.
	delete(fx.rpmBytes, "demo-1.2-3.x86_64.rpm")
	if err := p.scan(context.Background(), fx.remote(), store); err != nil {
		t.Fatalf("rescan: %v", err)
	}
	if rows, _ := store.ListRPMMetadataEntries(context.Background(), "acme-rpm"); len(rows) != 0 {
		t.Fatalf("want 0 rows after prune, got %d", len(rows))
	}
}

func TestGitHubAssetPatternFilter(t *testing.T) {
	fx := newGitHubFixture(t, true)
	fx.rpmBytes["other-9-9.aarch64.rpm"] = testsupport.MinimalRPM("other", "9", "9", "aarch64")
	p := newTestProvider()
	store := newFakeStore()
	remote := fx.remote()
	remote.Patterns = []string{`^demo-.*\.x86_64\.rpm$`}

	if err := p.scan(context.Background(), remote, store); err != nil {
		t.Fatalf("scan: %v", err)
	}
	rows, _ := store.ListRPMMetadataEntries(context.Background(), "acme-rpm")
	if len(rows) != 1 || rows[0].Name != "demo" {
		t.Fatalf("pattern filter failed, rows=%+v", rows)
	}
}
