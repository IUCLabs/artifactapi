package alpine

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha1"
	"encoding/base64"
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

	"golang.org/x/time/rate"

	"github.com/IUCLabs/artifactapi/internal/githubauth"
	"github.com/IUCLabs/artifactapi/internal/provider"
	"github.com/IUCLabs/artifactapi/pkg/models"
)

// gitHubProvider is the process-wide singleton for github_alpine. The background
// Syncer binds its shared rate limiter and work queue onto this instance so the
// request path and the syncer drive the same derive machinery.
var gitHubProvider = newGitHubProvider()

func init() {
	provider.Register(gitHubProvider)
}

// Tuning knobs for the no-precache control fetch. An .apk is up to three
// concatenated gzip streams (optional signature, control, data); the control
// stream carrying .PKGINFO sits near the front, so a small prefix reliably
// covers it.
const (
	defaultHeaderRangeInitial = 32 << 10 // 32 KiB — covers the control stream of almost every .apk
	defaultHeaderRangeMax     = 16 << 20 // 16 MiB — give up past this and skip the asset
	defaultReleasePageCap     = 10       // 100 releases/page * 10 pages

	defaultScanTimeout  = 10 * time.Minute
	defaultServeTimeout = 30 * time.Second
	defaultColdWait     = 8 * time.Second
)

// GitHubProvider is a metadata-only remote: it scans a GitHub repo's releases
// for .apk assets, derives per-asset .PKGINFO metadata via a ranged prefix fetch
// (never downloading whole packages), synthesizes a per-arch APKINDEX from that
// cached metadata, and redirects package downloads to a backend "releases_remote"
// (the generic github.com remote) that serves the actual bytes.
type GitHubProvider struct {
	client *http.Client

	headerInitial int64
	headerMax     int64
	pageCap       int
	scanTimeout   time.Duration
	serveTimeout  time.Duration
	coldWait      time.Duration

	limiter *rate.Limiter
	syncer  *Syncer

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

func (p *GitHubProvider) limiterWait(ctx context.Context) error {
	if p.limiter == nil {
		return nil
	}
	return p.limiter.Wait(ctx)
}

func (p *GitHubProvider) Type() models.PackageType { return models.PackageGitHubAlpine }

func (p *GitHubProvider) Classify(path string) provider.Mutability {
	if strings.HasSuffix(path, "APKINDEX.tar.gz") {
		return provider.Mutable
	}
	return provider.Immutable
}

func (p *GitHubProvider) ContentType(path string) string {
	switch {
	case strings.HasSuffix(path, ".apk"):
		return "application/vnd.android.package-archive"
	case strings.HasSuffix(path, ".tar.gz"):
		return "application/gzip"
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

// ServeRemote answers a request against a github_alpine remote. It refreshes the
// derived metadata (bounded by mutable_ttl), serves a synthesized per-arch
// APKINDEX.tar.gz, and 302-redirects .apk downloads to the backend
// releases_remote. Returns false only for paths it does not own.
func (p *GitHubProvider) ServeRemote(w http.ResponseWriter, r *http.Request, remote models.Remote, reqPath, proxyBaseURL string, store provider.RemoteMetadataStore) bool {
	p.onRequest(remote, store)

	// apk requests the index at "./<arch>/APKINDEX.tar.gz"; collapse the
	// dot-segment before matching, mirroring the local indexer.
	path := normalizeIndexPath(reqPath)

	if strings.HasSuffix(path, "APKINDEX.tar.gz") {
		p.serveIndex(w, r, remote, path, store)
		return true
	}

	if strings.HasSuffix(path, ".apk") {
		if remote.ReleasesRemote == "" {
			http.Error(w, "github_alpine remote has no releases_remote configured for downloads", http.StatusInternalServerError)
			return true
		}
		p.serveApkRedirect(w, r, remote, path, proxyBaseURL, store)
		return true
	}

	return false
}

// serveApkRedirect resolves an apk-reconstructed download path — apk builds
// "<arch>/<name>-<version>.apk" itself because APKINDEX carries no filename — to
// the real github-relative asset path stored on the metadata row, then redirects
// to the backend releases_remote. Passing the inbound path through verbatim would
// point at a nonexistent, allowlist-denied github.com path.
func (p *GitHubProvider) serveApkRedirect(w http.ResponseWriter, r *http.Request, remote models.Remote, path, proxyBaseURL string, store provider.RemoteMetadataStore) {
	arch := strings.TrimSuffix(path[:strings.LastIndex(path, "/")+1], "/")
	basename := path[strings.LastIndex(path, "/")+1:]
	if arch == "" || strings.Contains(arch, "/") {
		http.Error(w, "apk download must be requested per-arch: <arch>/<name>-<version>.apk", http.StatusNotFound)
		return
	}

	reader, ok := store.(provider.AlpineMetadataReader)
	if !ok {
		http.Error(w, "alpine metadata not available", http.StatusInternalServerError)
		return
	}
	sctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), p.serveTimeout)
	defer cancel()
	rows, err := reader.ListAlpineMetadataEntries(sctx, remote.Name)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	for _, row := range rows {
		if row.Arch == arch && row.Name+"-"+row.Version+".apk" == basename {
			loc := strings.TrimRight(proxyBaseURL, "/") + "/api/v1/remote/" + remote.ReleasesRemote + "/" + strings.TrimLeft(row.FilePath, "/")
			http.Redirect(w, r, loc, http.StatusFound)
			return
		}
	}
	http.Error(w, "package not found", http.StatusNotFound)
}

func (p *GitHubProvider) serveIndex(w http.ResponseWriter, r *http.Request, remote models.Remote, path string, store provider.RemoteMetadataStore) {
	arch := strings.TrimSuffix(path, "APKINDEX.tar.gz")
	arch = strings.Trim(arch, "/")
	if arch == "" || strings.Contains(arch, "/") {
		http.Error(w, "APKINDEX must be requested per-arch: <arch>/APKINDEX.tar.gz", http.StatusNotFound)
		return
	}

	// Serve on a context detached from the inbound request so a client disconnect
	// never cancels the metadata DB read and surfaces as a 500.
	sctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), p.serveTimeout)
	defer cancel()

	if p.syncer != nil && !p.ensurePrimed(sctx, remote, store) {
		w.Header().Set("Retry-After", "5")
		http.Error(w, "metadata is being prepared, retry shortly", http.StatusServiceUnavailable)
		return
	}

	reader, ok := store.(provider.AlpineMetadataReader)
	if !ok {
		http.Error(w, "alpine metadata not available", http.StatusInternalServerError)
		return
	}
	metas, err := reader.ListAlpineMetadataEntries(sctx, remote.Name)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			http.Error(w, "metadata read canceled", http.StatusServiceUnavailable)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	var filtered []provider.AlpineMetadata
	for _, m := range metas {
		if m.Arch == arch {
			filtered = append(filtered, m)
		}
	}

	w.Header().Set("Content-Type", "application/gzip")
	w.WriteHeader(http.StatusOK)
	w.Write(generateAPKIndex(filtered))
}

// onRequest keeps a remote's derived metadata fresh off the request path.
func (p *GitHubProvider) onRequest(remote models.Remote, store provider.RemoteMetadataStore) {
	if p.syncer != nil {
		p.syncer.enqueue(remote, false)
		return
	}
	p.refresh(remote, store)
}

// ensurePrimed returns true once the remote has at least one cached row. On an
// empty cache it enqueues a prime and polls briefly for it to land.
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
	reader, ok := store.(provider.AlpineMetadataReader)
	if !ok {
		return false
	}
	rows, err := reader.ListAlpineMetadataEntries(ctx, name)
	if err != nil {
		return false
	}
	return len(rows) == 0
}

// refresh brings the derived metadata up to date without coupling the scan to
// the inbound request (legacy inline path used without a syncer / in unit tests).
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

	if p.cacheEmpty(context.Background(), store, remote.Name) {
		p.runScan(remote, store)
		return
	}
	go p.runScan(remote, store)
}

func (p *GitHubProvider) runScan(remote models.Remote, store provider.RemoteMetadataStore) {
	defer func() {
		p.mu.Lock()
		delete(p.scanning, remote.Name)
		p.mu.Unlock()
	}()

	ctx, cancel := context.WithTimeout(context.Background(), p.scanTimeout)
	defer cancel()

	if err := p.scan(ctx, remote, store); err != nil {
		slog.Error("github_alpine: release scan failed", "remote", remote.Name, "error", err)
		return
	}

	p.mu.Lock()
	p.lastScan[remote.Name] = time.Now()
	p.mu.Unlock()
}

// scan runs a full unconditional derive. Retained for the legacy inline refresh
// path and existing tests; the syncer uses scanWithState.
func (p *GitHubProvider) scan(ctx context.Context, remote models.Remote, store provider.RemoteMetadataStore) error {
	_, _, err := p.scanWithState(ctx, remote, store, "")
	return err
}

// scanWithState derives metadata incrementally. It sends the prior releases-list
// ETag as a conditional request: a 304 means nothing changed. On a 200 it diffs
// the release assets against the cache, derives only new/changed assets, prunes
// assets that disappeared, and returns the new ETag.
func (p *GitHubProvider) scanWithState(ctx context.Context, remote models.Remote, store provider.RemoteMetadataStore, etag string) (newEtag string, changed bool, err error) {
	inserter, ok := store.(provider.AlpineMetadataStore)
	if !ok {
		return etag, false, errors.New("store does not support alpine metadata writes")
	}
	deleter, ok := store.(provider.AlpineMetadataDeleter)
	if !ok {
		return etag, false, errors.New("store does not support alpine metadata deletes")
	}
	reader, ok := store.(provider.AlpineMetadataReader)
	if !ok {
		return etag, false, errors.New("store does not support alpine metadata reads")
	}

	releases, newEtag, notModified, err := p.fetchReleases(ctx, remote, etag)
	if err != nil {
		return etag, false, err
	}
	if notModified {
		return etag, false, nil
	}

	existing, err := reader.ListAlpineMetadataEntries(ctx, remote.Name)
	if err != nil {
		return newEtag, false, err
	}
	existingByPath := make(map[string]provider.AlpineMetadata, len(existing))
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
			if !strings.HasSuffix(strings.ToLower(asset.Name), ".apk") {
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
				if asset.Digest == "" || cur.ContentHash == asset.Digest {
					continue
				}
				_ = deleter.DeleteAlpineMetadata(ctx, remote.Name, fp)
			}

			meta, err := p.deriveAsset(ctx, remote, asset, fp)
			if err != nil {
				slog.Warn("github_alpine: derive asset failed", "remote", remote.Name, "asset", asset.Name, "error", err)
				continue
			}
			if err := inserter.InsertAlpineMetadata(ctx, meta); err != nil {
				slog.Error("github_alpine: insert metadata failed", "remote", remote.Name, "asset", asset.Name, "error", err)
				continue
			}
			slog.Info("github_alpine: derived asset", "remote", remote.Name, "name", meta.Name, "version", meta.Version, "arch", meta.Arch)
		}
	}

	for fp := range existingByPath {
		if !seen[fp] {
			_ = deleter.DeleteAlpineMetadata(ctx, remote.Name, fp)
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

// fetchReleases lists a repo's releases, sending the prior ETag as If-None-Match
// on page 1 so an unchanged repo short-circuits to notModified. Every call waits
// on the shared limiter first.
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

func (p *GitHubProvider) deriveAsset(ctx context.Context, remote models.Remote, asset ghAsset, fp string) (*provider.AlpineMetadata, error) {
	meta, err := p.fetchPkginfo(ctx, remote, asset.BrowserDownloadURL)
	if err != nil {
		return nil, err
	}
	if meta.Name == "" || meta.Arch == "" {
		return nil, errors.New(".PKGINFO missing pkgname/arch")
	}

	meta.RepoName = remote.Name
	meta.FilePath = fp
	// S: the on-disk .apk size comes straight from the releases API, so we never
	// download the body just to size it.
	meta.DownloadSize = asset.Size
	// ContentHash records the GitHub asset digest (when present) purely so the
	// next scan can detect a changed asset; unlike deb it is not the index
	// checksum (that is the Q1 control-stream sum already set in fetchPkginfo).
	if asset.Digest != "" {
		meta.ContentHash = asset.Digest
	}
	return meta, nil
}

// fetchPkginfo pulls only the front of the .apk with a ranged GET and derives the
// .PKGINFO fields plus the apk pull checksum (C: = Q1 + base64(sha1(control gzip
// stream))). The control stream sits near the front, so a small prefix suffices;
// a prefix that truncates it doubles the range and retries.
func (p *GitHubProvider) fetchPkginfo(ctx context.Context, remote models.Remote, downloadURL string) (*provider.AlpineMetadata, error) {
	n := p.headerInitial
	for {
		body, full, err := p.rangeGet(ctx, remote, downloadURL, n)
		if err != nil {
			return nil, err
		}
		meta, complete, perr := pkginfoFromPrefix(body)
		if perr != nil {
			return nil, fmt.Errorf("parse apk .PKGINFO: %w", perr)
		}
		if complete {
			return meta, nil
		}
		if full || n >= p.headerMax {
			return nil, fmt.Errorf(".PKGINFO not found within %d bytes of %s", n, downloadURL)
		}
		n *= 2
		if n > p.headerMax {
			n = p.headerMax
		}
	}
}

// pkginfoFromPrefix parses the concatenated gzip streams present in a front
// prefix of an .apk. It walks each fully-covered gzip member until it finds the
// control stream (the one whose tar carries .PKGINFO), computes the Q1 pull
// checksum from that stream's raw bytes, and reads the .PKGINFO fields. A prefix
// too short to fully cover the control stream returns complete=false so the
// caller can widen the range.
func pkginfoFromPrefix(prefix []byte) (meta *provider.AlpineMetadata, complete bool, err error) {
	br := bytes.NewReader(prefix)
	zr, zerr := gzip.NewReader(br)
	if zerr != nil {
		if zerr == io.EOF || zerr == io.ErrUnexpectedEOF {
			return nil, false, nil
		}
		return nil, false, zerr
	}
	prev := 0
	for {
		zr.Multistream(false)
		out, rerr := io.ReadAll(zr)
		if rerr != nil {
			// A member truncated by the range boundary is not an error — widen.
			if rerr == io.ErrUnexpectedEOF || rerr == io.EOF {
				return nil, false, nil
			}
			return nil, false, rerr
		}
		end := len(prefix) - br.Len()
		raw := prefix[prev:end]

		if pkginfo, ok := pkginfoFromTar(out); ok {
			m := parsePkginfo(pkginfo)
			sum := sha1.Sum(raw)
			m.Checksum = "Q1" + base64.StdEncoding.EncodeToString(sum[:])
			return m, true, nil
		}

		prev = end
		if rsterr := zr.Reset(br); rsterr != nil {
			if rsterr == io.EOF {
				// No more complete members in the prefix; the control stream is
				// either not covered yet or genuinely absent — let the caller
				// decide by widening (or hitting the full-object guard).
				return nil, false, nil
			}
			if rsterr == io.ErrUnexpectedEOF {
				return nil, false, nil
			}
			return nil, false, rsterr
		}
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

// assetPath is the package's location relative to github.com — the path the
// backend releases_remote (base https://github.com) proxies. It doubles as the
// alpine_metadata key and the redirect target, so an .apk download resolves back
// to this remote and redirects to the backend.
func assetPath(asset ghAsset) string {
	u, err := url.Parse(asset.BrowserDownloadURL)
	if err != nil {
		return ""
	}
	return strings.TrimPrefix(u.Path, "/")
}

// githubHeaders builds the outbound headers for a GitHub request, attaching a
// bearer credential when one is available. A per-remote credential wins; absent
// that, the process-wide server credential is used; absent both, the request is
// unauthenticated.
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
