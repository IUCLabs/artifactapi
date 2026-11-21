package deb

import (
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

// fakeStore is an in-memory provider.RemoteMetadataStore + DebMetadataReader
// keyed by file_path, mirroring the (repo_name, file_path) uniqueness of the
// real deb_metadata table.
type fakeStore struct {
	mu   sync.Mutex
	rows map[string]provider.DebMetadata
}

func newFakeStore() *fakeStore { return &fakeStore{rows: map[string]provider.DebMetadata{}} }

func (f *fakeStore) InsertDebMetadata(_ context.Context, m *provider.DebMetadata) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.rows[m.FilePath]; ok {
		return nil // ON CONFLICT DO NOTHING
	}
	f.rows[m.FilePath] = *m
	return nil
}

func (f *fakeStore) DeleteDebMetadata(_ context.Context, _, filePath string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.rows, filePath)
	return nil
}

func (f *fakeStore) InsertRPMMetadata(context.Context, *provider.RPMMetadata) error { return nil }
func (f *fakeStore) DeleteRPMMetadata(context.Context, string, string) error        { return nil }
func (f *fakeStore) ListRPMMetadataEntries(context.Context, string) ([]provider.RPMMetadata, error) {
	return nil, nil
}

func (f *fakeStore) ListDebMetadataEntries(ctx context.Context, _ string) ([]provider.DebMetadata, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]provider.DebMetadata, 0, len(f.rows))
	for _, m := range f.rows {
		out = append(out, m)
	}
	return out, nil
}

// githubFixture serves the releases API and the .deb asset downloads (with Range
// support) for a set of packages. digest controls whether the asset carries a
// sha256 digest (no-download path) or not (compute path).
type githubFixture struct {
	srv         *httptest.Server
	debBytes    map[string][]byte
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
		debBytes:  map[string][]byte{},
		rangeHit:  map[string]int{},
		fullHit:   map[string]int{},
		assetFail: map[string]int{},
	}
	f.debBytes["demo_1.2-3_amd64.deb"] = testsupport.MinimalDeb("demo", "1.2-3", "amd64")

	mux := http.NewServeMux()
	mux.HandleFunc("/repos/acme/tools/releases", func(w http.ResponseWriter, r *http.Request) {
		page := r.URL.Query().Get("page")
		if page != "" && page != "1" {
			_, _ = w.Write([]byte("[]"))
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
		for name := range f.debBytes {
			a := map[string]any{
				"name":                 name,
				"size":                 len(f.debBytes[name]),
				"browser_download_url": f.srv.URL + "/acme/tools/releases/download/v1.2-3/" + name,
			}
			if withDigest {
				sum := sha256.Sum256(f.debBytes[name])
				a["digest"] = "sha256:" + hex.EncodeToString(sum[:])
			}
			assets = append(assets, a)
		}
		rel := []map[string]any{{"tag_name": "v1.2-3", "draft": false, "assets": assets}}
		_ = json.NewEncoder(w).Encode(rel)
	})
	mux.HandleFunc("/acme/tools/releases/download/", func(w http.ResponseWriter, r *http.Request) {
		name := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
		body, ok := f.debBytes[name]
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
			_, _ = w.Write(body)
			return
		}
		var end int
		_, _ = fmt.Sscanf(rng, "bytes=0-%d", &end)
		if end >= len(body)-1 {
			end = len(body) - 1
		}
		w.Header().Set("Content-Range", fmt.Sprintf("bytes 0-%d/%d", end, len(body)))
		w.Header().Set("Content-Length", strconv.Itoa(end+1))
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(body[:end+1])
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *githubFixture) remote() models.Remote {
	return models.Remote{
		Name:           "acme-deb",
		PackageType:    models.PackageGitHubDeb,
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

const demoPath = "acme/tools/releases/download/v1.2-3/demo_1.2-3_amd64.deb"

func TestGitHubScanDerivesControlFromPrefixAndDigest(t *testing.T) {
	fx := newGitHubFixture(t, true)
	p := newTestProvider()
	store := newFakeStore()

	if err := p.scan(context.Background(), fx.remote(), store); err != nil {
		t.Fatalf("scan: %v", err)
	}

	metas, _ := store.ListDebMetadataEntries(context.Background(), "acme-deb")
	if len(metas) != 1 {
		t.Fatalf("want 1 metadata row, got %d", len(metas))
	}
	m := metas[0]
	if m.Name != "demo" || m.Version != "1.2-3" || m.Architecture != "amd64" {
		t.Fatalf("bad control fields: %+v", m)
	}
	if m.FilePath != demoPath {
		t.Fatalf("FilePath = %q, want %q", m.FilePath, demoPath)
	}
	if int(m.Size) != len(fx.debBytes["demo_1.2-3_amd64.deb"]) {
		t.Fatalf("Size = %d, want %d", m.Size, len(fx.debBytes["demo_1.2-3_amd64.deb"]))
	}
	sum := sha256.Sum256(fx.debBytes["demo_1.2-3_amd64.deb"])
	if m.SHA256 != hex.EncodeToString(sum[:]) {
		t.Fatalf("SHA256 = %q, want digest", m.SHA256)
	}
	if m.ContentHash != "sha256:"+hex.EncodeToString(sum[:]) {
		t.Fatalf("ContentHash = %q", m.ContentHash)
	}
	if m.MD5 != "" {
		t.Fatalf("MD5 should be unset for metadata-only derive, got %q", m.MD5)
	}
	if fx.fullHit["demo_1.2-3_amd64.deb"] != 0 {
		t.Fatalf("expected no full download when digest present, got %d", fx.fullHit["demo_1.2-3_amd64.deb"])
	}
	if fx.rangeHit["demo_1.2-3_amd64.deb"] == 0 {
		t.Fatalf("expected ranged control fetch")
	}
	if !strings.Contains(m.Control, "Package: demo") {
		t.Fatalf("raw control not captured: %q", m.Control)
	}
}

func TestGitHubChecksumComputedWhenDigestAbsent(t *testing.T) {
	fx := newGitHubFixture(t, false)
	p := newTestProvider()
	store := newFakeStore()

	if err := p.scan(context.Background(), fx.remote(), store); err != nil {
		t.Fatalf("scan: %v", err)
	}
	metas, _ := store.ListDebMetadataEntries(context.Background(), "acme-deb")
	if len(metas) != 1 {
		t.Fatalf("want 1 row, got %d", len(metas))
	}
	sum := sha256.Sum256(fx.debBytes["demo_1.2-3_amd64.deb"])
	if metas[0].SHA256 != hex.EncodeToString(sum[:]) {
		t.Fatalf("computed checksum mismatch: %q", metas[0].SHA256)
	}
	if fx.fullHit["demo_1.2-3_amd64.deb"] == 0 {
		t.Fatalf("expected a full download to compute sha256 when digest absent")
	}
}

func TestGitHubServeRemoteIndexAndRedirect(t *testing.T) {
	fx := newGitHubFixture(t, true)
	p := newTestProvider()
	store := newFakeStore()
	remote := fx.remote()
	const proxyBase = "https://artifactapi.example"

	// Release is served and triggers the initial scan.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/remote/acme-deb/Release", nil)
	if !p.ServeRemote(rec, req, remote, "Release", proxyBase, store) {
		t.Fatal("ServeRemote did not handle Release")
	}
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "Architectures:") {
		t.Fatalf("Release bad: code=%d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "amd64") {
		t.Fatalf("Release missing arch: %s", rec.Body.String())
	}

	// Packages carries the package with a Filename that is the github-relative
	// download path (so it resolves back to this remote and redirects).
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/x", nil)
	if !p.ServeRemote(rec, req, remote, "Packages", proxyBase, store) {
		t.Fatal("ServeRemote did not handle Packages")
	}
	pkgs := rec.Body.String()
	if !strings.Contains(pkgs, "Package: demo") {
		t.Fatalf("Packages missing package: %s", pkgs)
	}
	if !strings.Contains(pkgs, "Filename: "+demoPath) {
		t.Fatalf("Packages missing/incorrect Filename: %s", pkgs)
	}
	if !strings.Contains(pkgs, "SHA256: ") {
		t.Fatalf("Packages missing SHA256: %s", pkgs)
	}
	if strings.Contains(pkgs, "MD5sum:") {
		t.Fatalf("Packages should omit empty MD5sum: %s", pkgs)
	}

	// Packages.gz decompresses to the same content.
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/x", nil)
	if !p.ServeRemote(rec, req, remote, "Packages.gz", proxyBase, store) {
		t.Fatal("ServeRemote did not handle Packages.gz")
	}
	gz, err := gzip.NewReader(rec.Body)
	if err != nil {
		t.Fatalf("gzip: %v", err)
	}
	unz, _ := io.ReadAll(gz)
	if !strings.Contains(string(unz), "Package: demo") {
		t.Fatalf("Packages.gz missing package: %s", unz)
	}

	// InRelease/Release.gpg 404 (unsigned, consumed via [trusted=yes]).
	for _, sp := range []string{"InRelease", "Release.gpg"} {
		rec = httptest.NewRecorder()
		req = httptest.NewRequest(http.MethodGet, "/x", nil)
		if !p.ServeRemote(rec, req, remote, sp, proxyBase, store) {
			t.Fatalf("ServeRemote did not handle %s", sp)
		}
		if rec.Code != http.StatusNotFound {
			t.Fatalf("%s want 404, got %d", sp, rec.Code)
		}
	}

	// A .deb request redirects to the backend releases_remote.
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/api/v1/remote/acme-deb/"+demoPath, nil)
	if !p.ServeRemote(rec, req, remote, demoPath, proxyBase, store) {
		t.Fatal("ServeRemote did not handle .deb")
	}
	if rec.Code != http.StatusFound {
		t.Fatalf("want 302, got %d", rec.Code)
	}
	wantLoc := proxyBase + "/api/v1/remote/github/" + demoPath
	if got := rec.Header().Get("Location"); got != wantLoc {
		t.Fatalf("Location = %q, want %q", got, wantLoc)
	}
}

// Real apt appends the flat-repo dist "./" verbatim, so the metadata-only remote
// receives "./Packages" / "./Release"; ServeRemote must collapse the dot-segment
// and synthesize the same index as the un-prefixed request.
func TestGitHubServeRemoteAptDotSegment(t *testing.T) {
	fx := newGitHubFixture(t, true)
	p := newTestProvider()
	store := newFakeStore()
	remote := fx.remote()
	const proxyBase = "https://artifactapi.example"

	serve := func(path string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/api/v1/remote/acme-deb/"+path, nil)
		if !p.ServeRemote(rec, req, remote, path, proxyBase, store) {
			t.Fatalf("ServeRemote did not handle %q", path)
		}
		return rec
	}

	// Packages is deterministic: byte-identical to the un-prefixed request.
	plain, dotted := serve("Packages"), serve("./Packages")
	if plain.Code != 200 || dotted.Code != 200 {
		t.Fatalf("Packages: plain=%d dotted=%d, want 200/200", plain.Code, dotted.Code)
	}
	if !strings.Contains(dotted.Body.String(), "Package: demo") {
		t.Fatalf("./Packages missing synthesized body: %s", dotted.Body.String())
	}
	if !bytes.Equal(plain.Body.Bytes(), dotted.Body.Bytes()) {
		t.Error("./Packages body differs from Packages body")
	}

	// Release carries a time.Now() Date: header; compare the rest.
	rPlain, rDotted := serve("Release"), serve("./Release")
	if rPlain.Code != 200 || rDotted.Code != 200 {
		t.Fatalf("Release: plain=%d dotted=%d, want 200/200", rPlain.Code, rDotted.Code)
	}
	if stripDate(rPlain.Body.String()) != stripDate(rDotted.Body.String()) {
		t.Error("./Release body differs from Release body (ignoring Date)")
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
	req := httptest.NewRequest(http.MethodGet, "/api/v1/remote/acme-deb/Packages", nil).WithContext(ctx)

	if !p.ServeRemote(rec, req, remote, "Packages", "https://x", store) {
		t.Fatal("ServeRemote did not handle Packages")
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("canceled request must serve cache, not error; got code=%d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "Package: demo") {
		t.Fatalf("expected Packages served from cache, got %s", rec.Body.String())
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
	if rows, _ := store.ListDebMetadataEntries(context.Background(), "acme-deb"); len(rows) != 1 {
		t.Fatalf("want 1 row after first scan, got %d", len(rows))
	}

	delete(fx.debBytes, "demo_1.2-3_amd64.deb")
	if err := p.scan(context.Background(), fx.remote(), store); err != nil {
		t.Fatalf("rescan: %v", err)
	}
	if rows, _ := store.ListDebMetadataEntries(context.Background(), "acme-deb"); len(rows) != 0 {
		t.Fatalf("want 0 rows after prune, got %d", len(rows))
	}
}

func TestGitHubAssetPatternFilter(t *testing.T) {
	fx := newGitHubFixture(t, true)
	fx.debBytes["other_9_arm64.deb"] = testsupport.MinimalDeb("other", "9", "arm64")
	p := newTestProvider()
	store := newFakeStore()
	remote := fx.remote()
	remote.Patterns = []string{`^demo_.*_amd64\.deb$`}

	if err := p.scan(context.Background(), remote, store); err != nil {
		t.Fatalf("scan: %v", err)
	}
	rows, _ := store.ListDebMetadataEntries(context.Background(), "acme-deb")
	if len(rows) != 1 || rows[0].Name != "demo" {
		t.Fatalf("pattern filter failed, rows=%+v", rows)
	}
}

func TestGitHubDeriveAssetMissingPackageIsInvalid(t *testing.T) {
	fx := newGitHubFixture(t, true)
	fx.debBytes["nameless_1_amd64.deb"] = testsupport.MinimalDeb("", "1", "amd64")
	asset := ghAsset{BrowserDownloadURL: fx.srv.URL + "/acme/tools/releases/download/v1.2-3/nameless_1_amd64.deb", Digest: "sha256:00"}
	_, err := newTestProvider().deriveAsset(context.Background(), fx.remote(), asset, "fp")
	if !errors.Is(err, provider.ErrInvalidPackage) {
		t.Fatalf("want ErrInvalidPackage, got %v", err)
	}
}
