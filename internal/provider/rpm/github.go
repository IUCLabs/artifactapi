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
	"golang.org/x/time/rate"

	"github.com/IUCLabs/artifactapi/internal/githubauth"
	"github.com/IUCLabs/artifactapi/internal/provider"
	"github.com/IUCLabs/artifactapi/pkg/models"
)

// gitHubProvider is the process-wide singleton. The background Syncer binds its
// shared rate limiter and work queue onto this instance so the request path and
// the syncer drive the same derive machinery.
var gitHubProvider = newGitHubProvider()

func init() {
	provider.Register(gitHubProvider)
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

	// defaultColdWait bounds how long a repodata request blocks waiting for a
	// just-enqueued prime to populate an empty cache before returning a
	// retryable 503. Kept short so a client never hangs on a rate-limited derive
	// of a large repo; small repos usually prime within this window.
	defaultColdWait = 8 * time.Second
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
	coldWait      time.Duration

	// limiter, when set by the Syncer, gates every GitHub HTTP call (releases
	// list + each ranged asset fetch) through a single process-wide token bucket.
	// nil means unlimited (direct provider use / unit tests).
	limiter *rate.Limiter
	// syncer, when set, routes freshness refresh and cold-start priming through
	// the shared background work queue instead of an inline per-replica scan.
	syncer *Syncer

	// serverCred overrides the process-wide GitHub credential for this provider
	// instance. nil falls back to githubauth.Server(); set directly in tests.
	serverCred githubauth.Credential

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
		coldWait:      defaultColdWait,
		scanning:      map[string]bool{},
		lastScan:      map[string]time.Time{},
	}
}

// limiterWait blocks until the shared rate limiter grants a token, or returns
// the context error if it is canceled first. A nil limiter is a no-op.
func (p *GitHubProvider) limiterWait(ctx context.Context) error {
	if p.limiter == nil {
		return nil
	}
	return p.limiter.Wait(ctx)
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

func (p *GitHubProvider) AuthHeaders(ctx context.Context, remote models.Remote) (http.Header, error) {
	return p.githubHeaders(ctx, remote, false)
}

// ServeRemote answers a request against a github_rpm remote. It refreshes the
// derived metadata (bounded by mutable_ttl), serves synthesized repodata, and
// 302-redirects .rpm downloads to the backend releases_remote. Returns false
// only for paths it does not own, letting the normal proxy path take over.
func (p *GitHubProvider) ServeRemote(w http.ResponseWriter, r *http.Request, remote models.Remote, path, proxyBaseURL string, store provider.RemoteMetadataStore) bool {
	p.onRequest(remote, store)

	if strings.HasPrefix(path, "repodata/") {
		// Serve repodata on a context detached from the inbound request: a
		// client disconnect (e.g. dnf makecache timing out) must never cancel
		// the metadata DB read and surface as a 500.
		sctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), p.serveTimeout)
		defer cancel()
		sr := r.WithContext(sctx)

		// Cold start: with the syncer wired, an empty cache means the prime has
		// not landed yet. Enqueue it and wait briefly rather than serving empty
		// repodata; if it still has not primed, return a retryable 503.
		if p.syncer != nil && !p.ensurePrimed(sctx, remote, store) {
			w.Header().Set("Retry-After", "5")
			http.Error(w, "metadata is being prepared, retry shortly", http.StatusServiceUnavailable)
			return true
		}

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

// onRequest keeps a remote's derived metadata fresh off the request path. With
// the background syncer wired it enqueues a deduped, rate-limited, lease-gated
// refresh and returns immediately; the request always serves the current cache.
// Without a syncer (direct provider use / unit tests) it falls back to the
// legacy inline single-flight scan.
func (p *GitHubProvider) onRequest(remote models.Remote, store provider.RemoteMetadataStore) {
	if p.syncer != nil {
		p.syncer.enqueue(remote, false)
		return
	}
	p.refresh(remote, store)
}

// ensurePrimed returns true once the remote has at least one cached metadata
// row. On an empty cache it enqueues a prime and polls briefly for it to land,
// so the very first client after a remote is created gets real repodata instead
// of an empty index or a blocking multi-minute derive. Returns false if the
// cache is still empty after the bounded wait.
func (p *GitHubProvider) ensurePrimed(ctx context.Context, remote models.Remote, store provider.RemoteMetadataStore) bool {
	if !p.cacheEmpty(ctx, store, remote.Name) {
		return true
	}
	if p.syncer != nil {
		p.syncer.enqueue(remote, true)
	}

	deadline := time.Now().Add(p.coldWait)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return false
		case <-time.After(400 * time.Millisecond):
		}
		if !p.cacheEmpty(ctx, store, remote.Name) {
			return true
		}
	}
	return false
}

func (p *GitHubProvider) cacheEmpty(ctx context.Context, store provider.RemoteMetadataStore, name string) bool {
	rows, err := store.ListRPMMetadataEntries(ctx, name)
	if err != nil {
		// Treat a failed read as "not empty" so a transient DB error becomes a
		// normal serve attempt (which reports its own error) rather than a 503.
		return false
	}
	return len(rows) == 0
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

// scan runs a full unconditional derive. Retained for the legacy inline refresh
// path and existing tests; the syncer uses scanWithState to pass and receive the
// releases-list ETag.
func (p *GitHubProvider) scan(ctx context.Context, remote models.Remote, store provider.RemoteMetadataStore) error {
	_, _, err := p.scanWithState(ctx, remote, store, "")
	return err
}

// scanWithState derives metadata incrementally. It sends the prior releases-list
// ETag as a conditional request: a 304 means nothing changed, so it returns
// (etag, changed=false) without a single asset fetch. On a 200 it diffs the
// release assets against the cache, derives only new/changed assets, prunes
// assets that disappeared, and returns the new ETag.
func (p *GitHubProvider) scanWithState(ctx context.Context, remote models.Remote, store provider.RemoteMetadataStore, etag string) (newEtag string, changed bool, err error) {
	releases, newEtag, notModified, err := p.fetchReleases(ctx, remote, etag)
	if err != nil {
		return etag, false, err
	}
	if notModified {
		return etag, false, nil
	}

	existing, err := store.ListRPMMetadataEntries(ctx, remote.Name)
	if err != nil {
		return newEtag, false, err
	}
	existingByPath := make(map[string]provider.RPMMetadata, len(existing))
	for _, m := range existing {
		existingByPath[m.FilePath] = m
	}

	allow, err := compilePatterns(remote.Patterns)
	if err != nil {
		return newEtag, false, err
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
	return newEtag, true, nil
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

// fetchReleases lists a repo's releases. It sends the prior ETag as
// If-None-Match on page 1 (the newest releases, where a new one first appears):
// a 304 there means the repo is unchanged, so it returns notModified without
// paging further — GitHub does not count 304 conditional responses against the
// rate limit, making an unchanged repo nearly free. On a 200 it captures the
// page-1 ETag and pages through the rest normally. Every call waits on the
// shared limiter first.
func (p *GitHubProvider) fetchReleases(ctx context.Context, remote models.Remote, etag string) (all []ghRelease, newEtag string, notModified bool, err error) {
	base := strings.TrimRight(remote.BaseURL, "/") + "/releases"
	for page := 1; page <= p.pageCap; page++ {
		u := fmt.Sprintf("%s?per_page=100&page=%d", base, page)
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			return nil, "", false, err
		}
		hdr, err := p.githubHeaders(ctx, remote, true)
		if err != nil {
			return nil, "", false, err
		}
		copyHeaders(req, hdr)
		if page == 1 && etag != "" {
			req.Header.Set("If-None-Match", etag)
		}

		if err := p.limiterWait(ctx); err != nil {
			return nil, "", false, err
		}
		resp, err := p.client.Do(req)
		if err != nil {
			return nil, "", false, err
		}
		if page == 1 && resp.StatusCode == http.StatusNotModified {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			return nil, etag, true, nil
		}
		body, err := io.ReadAll(resp.Body)
		respEtag := resp.Header.Get("ETag")
		resp.Body.Close()
		if err != nil {
			return nil, "", false, err
		}
		if resp.StatusCode != http.StatusOK {
			return nil, "", false, provider.NewUpstreamStatusError("github releases API "+u, resp)
		}
		if page == 1 {
			newEtag = respEtag
		}
		var releases []ghRelease
		if err := json.Unmarshal(body, &releases); err != nil {
			return nil, "", false, fmt.Errorf("decode releases: %w", err)
		}
		if len(releases) == 0 {
			break
		}
		all = append(all, releases...)
		if len(releases) < 100 {
			break
		}
	}
	return all, newEtag, false, nil
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
	hdr, err := p.githubHeaders(ctx, remote, false)
	if err != nil {
		return nil, false, err
	}
	copyHeaders(req, hdr)
	req.Header.Set("Range", fmt.Sprintf("bytes=0-%d", n-1))

	if err := p.limiterWait(ctx); err != nil {
		return nil, false, err
	}
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
	hdr, err := p.githubHeaders(ctx, remote, false)
	if err != nil {
		return "", err
	}
	copyHeaders(req, hdr)

	if err := p.limiterWait(ctx); err != nil {
		return "", err
	}
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

// githubHeaders builds the outbound headers for a GitHub request, attaching a
// bearer credential when one is available. A per-remote credential wins; absent
// that, the process-wide server credential is used; absent both, the request is
// unauthenticated (anonymous, subject to the 60/hr cap).
func (p *GitHubProvider) githubHeaders(ctx context.Context, remote models.Remote, api bool) (http.Header, error) {
	h := http.Header{}
	if api {
		h.Set("Accept", "application/vnd.github+json")
		h.Set("X-GitHub-Api-Version", "2022-11-28")
	}
	tok, err := p.githubToken(ctx, remote)
	if err != nil {
		return nil, err
	}
	if tok != "" {
		h.Set("Authorization", "Bearer "+tok)
	}
	return h, nil
}

// githubToken resolves the bearer token for a remote. Precedence: a per-remote
// credential (password, then username) overrides the server credential.
func (p *GitHubProvider) githubToken(ctx context.Context, remote models.Remote) (string, error) {
	if remote.Password != "" {
		return remote.Password, nil
	}
	if remote.Username != "" {
		return remote.Username, nil
	}
	if c := p.serverCredential(); c != nil {
		return c.Token(ctx)
	}
	return "", nil
}

// serverCredential returns this provider's server credential, defaulting to the
// process-wide one installed at startup.
func (p *GitHubProvider) serverCredential() githubauth.Credential {
	if p.serverCred != nil {
		return p.serverCred
	}
	return githubauth.Server()
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
