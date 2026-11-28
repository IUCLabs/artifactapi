package deb

import (
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
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/time/rate"

	"github.com/IUCLabs/artifactapi/internal/githubauth"
	"github.com/IUCLabs/artifactapi/internal/provider"
	"github.com/IUCLabs/artifactapi/pkg/models"
)

// gitHubProvider is the process-wide singleton for github_deb. The background
// Syncer binds its shared rate limiter and work queue onto this instance so the
// request path and the syncer drive the same derive machinery.
var gitHubProvider = newGitHubProvider()

func init() {
	provider.Register(gitHubProvider)
}

// Tuning knobs for the no-precache control fetch. A .deb is an ar archive whose
// control.tar member sits right after the tiny debian-binary member, so a small
// front prefix reliably covers it.
const (
	defaultHeaderRangeInitial = 32 << 10 // 32 KiB — covers control.tar of almost every .deb
	defaultHeaderRangeMax     = 16 << 20 // 16 MiB — give up past this and skip the asset
	defaultReleasePageCap     = 10       // 100 releases/page * 10 pages

	defaultScanTimeout  = 10 * time.Minute
	defaultServeTimeout = 30 * time.Second
	defaultColdWait     = 8 * time.Second
)

// GitHubProvider is a metadata-only remote: it scans a GitHub repo's releases
// for .deb assets, derives per-asset control metadata via a ranged prefix fetch
// (never downloading whole packages), synthesizes a flat apt repository from that
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

func (p *GitHubProvider) Type() models.PackageType { return models.PackageGitHubDeb }

func (p *GitHubProvider) Classify(path string) provider.Mutability {
	switch path {
	case "Packages", "Packages.gz", "Release", "InRelease", "Release.gpg":
		return provider.Mutable
	}
	return provider.Immutable
}

func (p *GitHubProvider) ContentType(path string) string {
	switch {
	case strings.HasSuffix(path, ".deb"):
		return "application/vnd.debian.binary-package"
	case strings.HasSuffix(path, ".gz"):
		return "application/gzip"
	case path == "Packages" || path == "Release" || path == "InRelease":
		return "text/plain"
	}
	return "application/octet-stream"
}

func (p *GitHubProvider) UpstreamURL(remote models.Remote, path string) string {
	return strings.TrimRight(remote.BaseURL, "/") + "/" + strings.TrimLeft(path, "/")
}

func (p *GitHubProvider) AuthHeaders(ctx context.Context, remote models.Remote) (http.Header, error) {
	return p.githubHeaders(ctx, remote, false)
}

// ServeRemote answers a request against a github_deb remote. It refreshes the
// derived metadata (bounded by mutable_ttl), serves a synthesized flat apt repo
// (Packages/Packages.gz/Release), 404s the signed index variants (the repo is
// consumed via [trusted=yes]), and 302-redirects .deb downloads to the backend
// releases_remote. Returns false only for paths it does not own.
func (p *GitHubProvider) ServeRemote(w http.ResponseWriter, r *http.Request, remote models.Remote, reqPath, proxyBaseURL string, store provider.RemoteMetadataStore) bool {
	p.onRequest(remote, store)

	// apt appends the flat-repo dist "./" verbatim, so it asks for "./Packages"
	// etc.; collapse the dot-segment before matching the synthesized index.
	path := normalizeIndexPath(reqPath)

	switch path {
	case "Packages", "Packages.gz", "Release":
		p.serveIndex(w, r, remote, path, store)
		return true
	case "InRelease", "Release.gpg":
		// Unsigned flat repo: apt consumes it with [trusted=yes]. Signal absence
		// so apt falls back to the plain Release without waiting on a signature.
		http.Error(w, "not found", http.StatusNotFound)
		return true
	}

	if strings.HasSuffix(path, ".deb") {
		if remote.ReleasesRemote == "" {
			http.Error(w, "github_deb remote has no releases_remote configured for downloads", http.StatusInternalServerError)
			return true
		}
		loc := strings.TrimRight(proxyBaseURL, "/") + "/api/v1/remote/" + remote.ReleasesRemote + "/" + strings.TrimLeft(path, "/")
		http.Redirect(w, r, loc, http.StatusFound)
		return true
	}

	return false
}

func (p *GitHubProvider) serveIndex(w http.ResponseWriter, r *http.Request, remote models.Remote, path string, store provider.RemoteMetadataStore) {
	// Serve on a context detached from the inbound request so a client disconnect
	// never cancels the metadata DB read and surfaces as a 500.
	sctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), p.serveTimeout)
	defer cancel()

	if p.syncer != nil && !p.ensurePrimed(sctx, remote, store) {
		w.Header().Set("Retry-After", "5")
		http.Error(w, "metadata is being prepared, retry shortly", http.StatusServiceUnavailable)
		return
	}

	reader, ok := store.(provider.DebMetadataReader)
	if !ok {
		http.Error(w, "deb metadata not available", http.StatusInternalServerError)
		return
	}
	metas, err := reader.ListDebMetadataEntries(sctx, remote.Name)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			http.Error(w, "metadata read canceled", http.StatusServiceUnavailable)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	switch path {
	case "Packages":
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(generatePackages(metas))
	case "Packages.gz":
		w.Header().Set("Content-Type", "application/gzip")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(gzipBytes(generatePackages(metas)))
	case "Release":
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(generateRelease(metas))
	}
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
	reader, ok := store.(provider.DebMetadataReader)
	if !ok {
		return false
	}
	rows, err := reader.ListDebMetadataEntries(ctx, name)
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
		slog.Error("github_deb: release scan failed", "remote", remote.Name, "error", err)
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
	releases, newEtag, notModified, err := p.fetchReleases(ctx, remote, etag)
	if err != nil {
		return etag, false, err
	}
	if notModified {
		return etag, false, nil
	}

	reader, ok := store.(provider.DebMetadataReader)
	if !ok {
		return newEtag, false, errors.New("store does not support deb metadata reads")
	}
	existing, err := reader.ListDebMetadataEntries(ctx, remote.Name)
	if err != nil {
		return newEtag, false, err
	}
	existingByPath := make(map[string]provider.DebMetadata, len(existing))
	for _, m := range existing {
		existingByPath[m.FilePath] = m
	}

	allow, err := compilePatterns(remote.Patterns)
	if err != nil {
		return newEtag, false, err
	}

	// A transiently failed asset fails the scan so the ETag is not advanced and
	// the retry re-derives it; an invalid package is skipped as no retry fixes it.
	var failed []error
	seen := map[string]bool{}
	for _, rel := range releases {
		if rel.Draft {
			continue
		}
		for _, asset := range rel.Assets {
			if !strings.HasSuffix(strings.ToLower(asset.Name), ".deb") {
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
				_ = store.DeleteDebMetadata(ctx, remote.Name, fp)
			}

			meta, err := p.deriveAsset(ctx, remote, asset, fp)
			if err != nil {
				slog.Warn("github_deb: derive asset failed", "remote", remote.Name, "asset", asset.Name, "error", err)
				if !errors.Is(err, provider.ErrInvalidPackage) {
					failed = append(failed, fmt.Errorf("%s: %w", asset.Name, err))
				}
				continue
			}
			if err := store.InsertDebMetadata(ctx, meta); err != nil {
				slog.Error("github_deb: insert metadata failed", "remote", remote.Name, "asset", asset.Name, "error", err)
				failed = append(failed, fmt.Errorf("%s: %w", asset.Name, err))
				continue
			}
			slog.Info("github_deb: derived asset", "remote", remote.Name, "name", meta.Name, "version", meta.Version, "arch", meta.Architecture)
		}
	}

	for fp := range existingByPath {
		if !seen[fp] {
			_ = store.DeleteDebMetadata(ctx, remote.Name, fp)
		}
	}
	return newEtag, true, errors.Join(failed...)
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
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			return nil, etag, true, nil
		}
		body, err := io.ReadAll(resp.Body)
		respEtag := resp.Header.Get("ETag")
		_ = resp.Body.Close()
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

func (p *GitHubProvider) deriveAsset(ctx context.Context, remote models.Remote, asset ghAsset, fp string) (*provider.DebMetadata, error) {
	control, err := p.fetchControl(ctx, remote, asset.BrowserDownloadURL)
	if err != nil {
		return nil, err
	}
	fields := parseControlFields(control)

	meta := &provider.DebMetadata{
		RepoName:     remote.Name,
		FilePath:     fp,
		Name:         fields["Package"],
		Version:      fields["Version"],
		Architecture: fields["Architecture"],
		Control:      strings.TrimRight(control, "\n"),
		Size:         asset.Size,
	}
	if meta.Name == "" {
		return nil, fmt.Errorf("%w: control missing Package field", provider.ErrInvalidPackage)
	}

	// The Packages SHA256 must be the sha256 of the whole .deb. Prefer GitHub's
	// asset digest so we never download the body; only when it is absent (or not
	// sha256) do we stream the asset once. MD5sum is left unset — apt verifies the
	// download against SHA256 alone under [trusted=yes].
	if h, ok := sha256FromDigest(asset.Digest); ok {
		meta.ContentHash = "sha256:" + h
		meta.SHA256 = h
	} else {
		h, err := p.computeSHA256(ctx, remote, asset.BrowserDownloadURL)
		if err != nil {
			return nil, fmt.Errorf("compute sha256: %w", err)
		}
		meta.ContentHash = "sha256:" + h
		meta.SHA256 = h
	}

	return meta, nil
}

// fetchControl pulls only the front of the .deb with a ranged GET and extracts
// the control paragraph from it. control.tar sits right after the tiny
// debian-binary member, so a small prefix suffices; a prefix that truncates the
// control member doubles the range and retries.
func (p *GitHubProvider) fetchControl(ctx context.Context, remote models.Remote, downloadURL string) (string, error) {
	n := p.headerInitial
	for {
		body, full, err := p.rangeGet(ctx, remote, downloadURL, n)
		if err != nil {
			return "", err
		}
		control, complete, perr := controlFromPrefix(body)
		if perr != nil {
			return "", fmt.Errorf("%w: parse deb control: %w", provider.ErrInvalidPackage, perr)
		}
		if complete {
			return control, nil
		}
		if full || n >= p.headerMax {
			return "", fmt.Errorf("%w: control.tar not found within %d bytes of %s", provider.ErrInvalidPackage, n, downloadURL)
		}
		n *= 2
		if n > p.headerMax {
			n = p.headerMax
		}
	}
}

// controlFromPrefix parses the ar members present in a front prefix of a .deb.
// It returns the ./control paragraph once control.tar.* is fully covered
// (complete=true); a prefix too short to cover it returns complete=false so the
// caller can widen the range. Later members (data.tar.*) are ignored.
func controlFromPrefix(prefix []byte) (control string, complete bool, err error) {
	const magic = "!<arch>\n"
	if len(prefix) < len(magic) {
		return "", false, nil
	}
	if string(prefix[:len(magic)]) != magic {
		return "", false, errors.New("not an ar archive")
	}
	off := len(magic)
	for {
		if off+60 > len(prefix) {
			return "", false, nil
		}
		hdr := prefix[off : off+60]
		off += 60
		name := strings.TrimSuffix(strings.TrimRight(string(hdr[0:16]), " "), "/")
		size, err := strconv.ParseInt(strings.TrimSpace(string(hdr[48:58])), 10, 64)
		if err != nil {
			return "", false, fmt.Errorf("bad ar size for %q: %w", name, err)
		}
		if strings.HasPrefix(name, "control.tar") {
			if off+int(size) > len(prefix) {
				return "", false, nil
			}
			tarBytes, err := decompress(name, prefix[off:off+int(size)])
			if err != nil {
				return "", false, err
			}
			c, err := readControlParagraph(tarBytes)
			if err != nil {
				return "", false, err
			}
			return c, true, nil
		}
		if off+int(size) > len(prefix) {
			return "", false, nil
		}
		off += int(size)
		if size%2 == 1 {
			off++
		}
	}
}

// rangeGet returns the first n bytes of downloadURL. full is true when the
// response holds the whole object.
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
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
		return nil, false, fmt.Errorf("range GET: %w", provider.AssetStatusError(downloadURL, resp))
	}
	return provider.ReadPrefix(resp, n)
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
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("GET: %w", provider.AssetStatusError(downloadURL, resp))
	}

	h := sha256.New()
	if _, err := io.Copy(h, resp.Body); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// assetPath is the package's location relative to github.com — the path the
// backend releases_remote (base https://github.com) proxies. It doubles as the
// deb_metadata key and the Filename field in the Packages index, so a .deb
// download resolves back to this remote and redirects to the backend.
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

// githubToken resolves the bearer token for a remote; see githubauth.RemoteToken.
func (p *GitHubProvider) githubToken(ctx context.Context, remote models.Remote) (string, error) {
	return githubauth.RemoteToken(ctx, githubauth.AppRegistry(), remote, p.serverCredential())
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
