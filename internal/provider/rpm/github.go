package rpm

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	rpmlib "github.com/cavaliergopher/rpm"

	"github.com/IUCLabs/artifactapi/internal/provider"
	"github.com/IUCLabs/artifactapi/pkg/models"
)

func init() {
	provider.Register(newGitHubProvider())
}

// Tuning knobs for the no-precache header fetch. Fields (not consts) so tests
// can shrink them against small fixtures.
const (
	defaultHeaderRangeInitial = 1 << 20  // 1 MiB — covers the header of almost every RPM
	defaultHeaderRangeMax     = 16 << 20 // 16 MiB — give up past this and skip the asset
	defaultReleasePageCap     = 10       // 100 releases/page * 10 pages

	// defaultScanTimeout bounds a detached background scan (which may do one
	// ranged fetch per asset across every release) so it can never run forever.
	defaultScanTimeout = 10 * time.Minute
	// defaultServeTimeout bounds a repodata DB read served on a detached context.
	defaultServeTimeout = 30 * time.Second
)

// GitHubProvider is a metadata-only remote: it scans a GitHub repo's releases
// for .rpm assets, derives per-asset RPM metadata via a ranged header fetch
// (never downloading whole packages), synthesizes yum repodata from that cached
// metadata, and redirects package downloads to a backend "releases_remote"
// (the generic github.com remote) that serves the actual bytes.
type GitHubProvider struct {
	client *http.Client

	headerInitial int64
	headerMax     int64
	pageCap       int
	scanTimeout   time.Duration
	serveTimeout  time.Duration

	mu       sync.Mutex
	scanning map[string]bool
	lastScan map[string]time.Time
}

func newGitHubProvider() *GitHubProvider {
	return &GitHubProvider{
		client:        &http.Client{},
		headerInitial: defaultHeaderRangeInitial,
		headerMax:     defaultHeaderRangeMax,
		pageCap:       defaultReleasePageCap,
		scanTimeout:   defaultScanTimeout,
		serveTimeout:  defaultServeTimeout,
		scanning:      map[string]bool{},
		lastScan:      map[string]time.Time{},
	}
}

func (p *GitHubProvider) Type() models.PackageType { return models.PackageGitHubRPM }

// Classify/ContentType/UpstreamURL/RewriteResponse/AuthHeaders satisfy the
// Provider interface. The proxy engine never reaches them for this type because
// ServeRemote handles every request, but they must exist for registry lookup.
func (p *GitHubProvider) Classify(path string) provider.Mutability {
	if strings.HasPrefix(path, "repodata/") {
		return provider.Mutable
	}
	return provider.Immutable
}

func (p *GitHubProvider) ContentType(path string) string {
	switch {
	case strings.HasSuffix(path, ".rpm"):
		return "application/x-rpm"
	case strings.HasSuffix(path, ".xml.gz"):
		return "application/gzip"
	case strings.HasSuffix(path, ".xml"):
		return "application/xml"
	}
	return "application/octet-stream"
}

func (p *GitHubProvider) UpstreamURL(remote models.Remote, path string) string {
	return strings.TrimRight(remote.BaseURL, "/") + "/" + strings.TrimLeft(path, "/")
}

func (p *GitHubProvider) RewriteResponse(_ []byte, _ models.Remote, _ string) ([]byte, error) {
	return nil, nil
}

func (p *GitHubProvider) AuthHeaders(_ context.Context, remote models.Remote) (http.Header, error) {
	return githubHeaders(remote, false), nil
}

// ServeRemote answers a request against a github_rpm remote. It refreshes the
// derived metadata (bounded by mutable_ttl), serves synthesized repodata, and
// 302-redirects .rpm downloads to the backend releases_remote. Returns false
// only for paths it does not own, letting the normal proxy path take over.
func (p *GitHubProvider) ServeRemote(w http.ResponseWriter, r *http.Request, remote models.Remote, path, proxyBaseURL string, store provider.RemoteMetadataStore) bool {
	p.refresh(remote, store)

	if strings.HasPrefix(path, "repodata/") {
		// Serve repodata on a context detached from the inbound request: a
		// client disconnect (e.g. dnf makecache timing out) must never cancel
		// the metadata DB read and surface as a 500.
		sctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), p.serveTimeout)
		defer cancel()
		sr := r.WithContext(sctx)

		tail := strings.TrimPrefix(path, "repodata/")
		lp := &Provider{}
		switch {
		case tail == "repomd.xml":
			lp.serveRepomd(w, sr, store, remote.Name)
		case strings.HasSuffix(tail, "-primary.xml.gz"):
			lp.servePrimary(w, sr, store, remote.Name)
		case strings.HasSuffix(tail, "-filelists.xml.gz"):
			lp.serveFilelists(w, sr, store, remote.Name)
		case strings.HasSuffix(tail, "-other.xml.gz"):
			lp.serveOther(w, sr, store, remote.Name)
		default:
			http.Error(w, "not found", http.StatusNotFound)
		}
		return true
	}

	if strings.HasSuffix(path, ".rpm") {
		if remote.ReleasesRemote == "" {
			http.Error(w, "github_rpm remote has no releases_remote configured for downloads", http.StatusInternalServerError)
			return true
		}
		loc := strings.TrimRight(proxyBaseURL, "/") + "/api/v1/remote/" + remote.ReleasesRemote + "/" + strings.TrimLeft(path, "/")
		http.Redirect(w, r, loc, http.StatusFound)
		return true
	}

	return false
}

// refresh brings the derived metadata up to date without coupling the scan to
// the inbound request. When the cache is stale it single-flights a scan: if the
// cache already holds rows the scan runs in the background and the caller serves
// the current cache immediately; only a completely empty cache blocks on a
// bounded first scan (so the first client sees packages rather than an empty or
// 500 repodata).
func (p *GitHubProvider) refresh(remote models.Remote, store provider.RemoteMetadataStore) {
	ttl := time.Duration(remote.MutableTTL) * time.Second
	if ttl <= 0 {
		ttl = 5 * time.Minute
	}

	p.mu.Lock()
	last, ok := p.lastScan[remote.Name]
	fresh := ok && time.Since(last) < ttl
	if fresh || p.scanning[remote.Name] {
		p.mu.Unlock()
		return
	}
	p.scanning[remote.Name] = true
	p.mu.Unlock()

	empty := true
	if rows, err := store.ListRPMMetadataEntries(context.Background(), remote.Name); err == nil {
		empty = len(rows) == 0
	}

	if empty {
		p.runScan(remote, store)
		return
	}
	go p.runScan(remote, store)
}

// runScan derives metadata on a detached, bounded context so a client cancel
// can neither abort the shared derive nor poison the metadata read. The caller
// must have already claimed the single-flight slot (scanning[name] = true).
func (p *GitHubProvider) runScan(remote models.Remote, store provider.RemoteMetadataStore) {
	defer func() {
		p.mu.Lock()
		delete(p.scanning, remote.Name)
		p.mu.Unlock()
	}()

	ctx, cancel := context.WithTimeout(context.Background(), p.scanTimeout)
	defer cancel()

	if err := p.scan(ctx, remote, store); err != nil {
		// Keep serving whatever metadata is already cached rather than 500ing.
		slog.Error("github_rpm: release scan failed", "remote", remote.Name, "error", err)
		return
	}

	p.mu.Lock()
	p.lastScan[remote.Name] = time.Now()
	p.mu.Unlock()
}

func (p *GitHubProvider) scan(ctx context.Context, remote models.Remote, store provider.RemoteMetadataStore) error {
	releases, err := p.fetchReleases(ctx, remote)
	if err != nil {
		return err
	}

	existing, err := store.ListRPMMetadataEntries(ctx, remote.Name)
	if err != nil {
		return err
	}
	existingByPath := make(map[string]provider.RPMMetadata, len(existing))
	for _, m := range existing {
		existingByPath[m.FilePath] = m
	}

	allow, err := compilePatterns(remote.Patterns)
	if err != nil {
		return err
	}

	seen := map[string]bool{}
	for _, rel := range releases {
		if rel.Draft {
			continue
		}
		for _, asset := range rel.Assets {
			if !strings.HasSuffix(strings.ToLower(asset.Name), ".rpm") {
				continue
			}
			if !matchesAny(allow, asset.Name) {
				continue
			}
			fp := assetPath(asset)
			if fp == "" {
				continue
			}
			seen[fp] = true

			if cur, ok := existingByPath[fp]; ok {
				// Assets are effectively immutable; only re-derive when the
				// upstream digest is known and no longer matches what we cached.
				if asset.Digest == "" || cur.ContentHash == asset.Digest {
					continue
				}
				_ = store.DeleteRPMMetadata(ctx, remote.Name, fp)
			}

			meta, err := p.deriveAsset(ctx, remote, asset, fp)
			if err != nil {
				slog.Warn("github_rpm: derive asset failed", "remote", remote.Name, "asset", asset.Name, "error", err)
				continue
			}
			if err := store.InsertRPMMetadata(ctx, meta); err != nil {
				slog.Error("github_rpm: insert metadata failed", "remote", remote.Name, "asset", asset.Name, "error", err)
				continue
			}
			slog.Info("github_rpm: derived asset", "remote", remote.Name, "name", meta.Name, "version", meta.Version, "arch", meta.Arch)
		}
	}

	for fp := range existingByPath {
		if !seen[fp] {
			_ = store.DeleteRPMMetadata(ctx, remote.Name, fp)
		}
	}
	return nil
}

type ghRelease struct {
	TagName string    `json:"tag_name"`
	Draft   bool      `json:"draft"`
	Assets  []ghAsset `json:"assets"`
}

type ghAsset struct {
	Name               string `json:"name"`
	Size               int64  `json:"size"`
	BrowserDownloadURL string `json:"browser_download_url"`
	Digest             string `json:"digest"`
}

func (p *GitHubProvider) fetchReleases(ctx context.Context, remote models.Remote) ([]ghRelease, error) {
	base := strings.TrimRight(remote.BaseURL, "/") + "/releases"
	var all []ghRelease
	for page := 1; page <= p.pageCap; page++ {
		u := fmt.Sprintf("%s?per_page=100&page=%d", base, page)
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			return nil, err
		}
		copyHeaders(req, githubHeaders(remote, true))

		resp, err := p.client.Do(req)
		if err != nil {
			return nil, err
		}
		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			return nil, err
		}
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("github releases API %s: status %d", u, resp.StatusCode)
		}
		var releases []ghRelease
		if err := json.Unmarshal(body, &releases); err != nil {
			return nil, fmt.Errorf("decode releases: %w", err)
		}
		if len(releases) == 0 {
			break
		}
		all = append(all, releases...)
		if len(releases) < 100 {
			break
		}
	}
	return all, nil
}

func (p *GitHubProvider) deriveAsset(ctx context.Context, remote models.Remote, asset ghAsset, fp string) (*provider.RPMMetadata, error) {
	pkg, err := p.fetchHeader(ctx, remote, asset.BrowserDownloadURL)
	if err != nil {
		return nil, err
	}

	meta := &provider.RPMMetadata{
		RepoName:      remote.Name,
		FilePath:      fp,
		Name:          pkg.Name(),
		Epoch:         pkg.Epoch(),
		Version:       pkg.Version(),
		Release:       pkg.Release(),
		Arch:          pkg.Architecture(),
		Summary:       pkg.Summary(),
		Description:   pkg.Description(),
		RPMSize:       asset.Size,
		InstalledSize: int64(pkg.Size()),
		License:       pkg.License(),
		Vendor:        pkg.Vendor(),
		Group:         firstGroup(pkg.Groups()),
		BuildHost:     pkg.BuildHost(),
		SourceRPM:     pkg.SourceRPM(),
		URL:           pkg.URL(),
		Packager:      pkg.Packager(),
	}

	for _, d := range pkg.Requires() {
		meta.Requires = append(meta.Requires, rpmDepFromEntry(d))
	}
	for _, d := range pkg.Provides() {
		meta.Provides = append(meta.Provides, rpmDepFromEntry(d))
	}
	for _, d := range pkg.Conflicts() {
		meta.Conflicts = append(meta.Conflicts, rpmDepFromEntry(d))
	}
	for _, d := range pkg.Obsoletes() {
		meta.Obsoletes = append(meta.Obsoletes, rpmDepFromEntry(d))
	}
	for _, f := range pkg.Files() {
		rf := provider.RPMFile{Path: f.Name()}
		if f.IsDir() {
			rf.Type = "dir"
		}
		meta.Files = append(meta.Files, rf)
	}

	if meta.Requires == nil {
		meta.Requires = []provider.RPMDep{}
	}
	if meta.Provides == nil {
		meta.Provides = []provider.RPMDep{}
	}
	if meta.Conflicts == nil {
		meta.Conflicts = []provider.RPMDep{}
	}
	if meta.Obsoletes == nil {
		meta.Obsoletes = []provider.RPMDep{}
	}
	if meta.Files == nil {
		meta.Files = []provider.RPMFile{}
	}
	meta.Changelogs = []provider.RPMChangelog{}

	// The primary.xml pkgid checksum must be the sha256 of the whole package.
	// Prefer GitHub's asset digest so we never download the body; only when it
	// is absent (or not sha256) do we stream the asset once to compute it.
	if h, ok := sha256FromDigest(asset.Digest); ok {
		meta.ContentHash = "sha256:" + h
	} else {
		h, err := p.computeSHA256(ctx, remote, asset.BrowserDownloadURL)
		if err != nil {
			return nil, fmt.Errorf("compute sha256: %w", err)
		}
		meta.ContentHash = "sha256:" + h
	}

	return meta, nil
}

// fetchHeader pulls only the front of the package with a ranged GET and parses
// the RPM header from it. The header sits before the payload, so a small prefix
// is enough; on a truncated-header parse error it doubles the range and retries.
func (p *GitHubProvider) fetchHeader(ctx context.Context, remote models.Remote, downloadURL string) (*rpmlib.Package, error) {
	n := p.headerInitial
	for {
		body, full, err := p.rangeGet(ctx, remote, downloadURL, n)
		if err != nil {
			return nil, err
		}
		pkg, perr := rpmlib.Read(bytes.NewReader(body))
		if perr == nil {
			return pkg, nil
		}
		truncated := errors.Is(perr, io.ErrUnexpectedEOF) || errors.Is(perr, io.EOF)
		if truncated && !full && n < p.headerMax {
			n *= 2
			if n > p.headerMax {
				n = p.headerMax
			}
			continue
		}
		return nil, fmt.Errorf("parse rpm header: %w", perr)
	}
}

// rangeGet returns the first n bytes of downloadURL. full is true when the
// response body was shorter than n (i.e. we already have the whole object).
func (p *GitHubProvider) rangeGet(ctx context.Context, remote models.Remote, downloadURL string, n int64) ([]byte, bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, downloadURL, nil)
	if err != nil {
		return nil, false, err
	}
	copyHeaders(req, githubHeaders(remote, false))
	req.Header.Set("Range", fmt.Sprintf("bytes=0-%d", n-1))

	resp, err := p.client.Do(req)
	if err != nil {
		return nil, false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
		return nil, false, fmt.Errorf("range GET %s: status %d", downloadURL, resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, n))
	if err != nil {
		return nil, false, err
	}
	full := int64(len(body)) < n
	return body, full, nil
}

func (p *GitHubProvider) computeSHA256(ctx context.Context, remote models.Remote, downloadURL string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, downloadURL, nil)
	if err != nil {
		return "", err
	}
	copyHeaders(req, githubHeaders(remote, false))

	resp, err := p.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("GET %s: status %d", downloadURL, resp.StatusCode)
	}

	h := sha256.New()
	if _, err := io.Copy(h, resp.Body); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// assetPath is the package's location relative to github.com — the path the
// backend releases_remote (base https://github.com) proxies. It doubles as the
// rpm_metadata key and the <location href> in primary.xml.
func assetPath(asset ghAsset) string {
	u, err := url.Parse(asset.BrowserDownloadURL)
	if err != nil {
		return ""
	}
	return strings.TrimPrefix(u.Path, "/")
}

func sha256FromDigest(digest string) (string, bool) {
	if strings.HasPrefix(digest, "sha256:") {
		return strings.TrimPrefix(digest, "sha256:"), true
	}
	return "", false
}

func githubHeaders(remote models.Remote, api bool) http.Header {
	h := http.Header{}
	if api {
		h.Set("Accept", "application/vnd.github+json")
		h.Set("X-GitHub-Api-Version", "2022-11-28")
	}
	if tok := githubToken(remote); tok != "" {
		h.Set("Authorization", "Bearer "+tok)
	}
	return h
}

func githubToken(remote models.Remote) string {
	if remote.Password != "" {
		return remote.Password
	}
	return remote.Username
}

func copyHeaders(req *http.Request, h http.Header) {
	for k, vals := range h {
		for _, v := range vals {
			req.Header.Add(k, v)
		}
	}
}

func compilePatterns(patterns []string) ([]*regexp.Regexp, error) {
	var out []*regexp.Regexp
	for _, p := range patterns {
		re, err := regexp.Compile(p)
		if err != nil {
			return nil, fmt.Errorf("invalid pattern %q: %w", p, err)
		}
		out = append(out, re)
	}
	return out, nil
}

func matchesAny(res []*regexp.Regexp, s string) bool {
	if len(res) == 0 {
		return true
	}
	for _, re := range res {
		if re.MatchString(s) {
			return true
		}
	}
	return false
}
