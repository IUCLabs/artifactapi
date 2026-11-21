package virtual

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/IUCLabs/artifactapi/internal/database"
	"github.com/IUCLabs/artifactapi/internal/provider"
	"github.com/IUCLabs/artifactapi/internal/proxy"
	"github.com/IUCLabs/artifactapi/pkg/models"
	"golang.org/x/sync/errgroup"
	"golang.org/x/sync/singleflight"
)

type Engine struct {
	db          *database.DB
	proxyEngine *proxy.Engine
	getRemote   func(context.Context, string) (*models.Remote, error)
	rpmMember   func(ctx context.Context, name string, parallel bool) (*RPMMember, error)
	debMember   func(ctx context.Context, name, base string, parallel bool) (*DebMember, error)
	apkMember   func(ctx context.Context, name, dir string) (*AlpineMember, error)
	npmMember   func(ctx context.Context, name, pkg string) (*NPMMember, error)
	memberPath  func(ctx context.Context, remote models.Remote, path string) ([]byte, error)

	// NPMLocal returns a local npm repo's packument with tarball paths relative
	// to the repo, or ErrNotFound.
	NPMLocal func(ctx context.Context, repo, pkg string) ([]byte, error)

	gemsMember func(ctx context.Context, name, path string) (*GemsMember, error)

	// GemsLocal returns a local gems repo's versions, names or info/<gem>, or
	// ErrNotFound.
	GemsLocal func(ctx context.Context, repo, path string) ([]byte, error)

	// AnsibleLocal returns a local ansible repo's collection, full versions list
	// or version detail with root-relative URLs, or ErrNotFound.
	AnsibleLocal func(ctx context.Context, repo, path string) ([]byte, error)
	ansibleDoc   func(ctx context.Context, remote models.Remote, path string) ([]byte, error)

	goMember func(ctx context.Context, name, path string) (*GoMember, error)

	puppetDoc func(ctx context.Context, remote models.Remote, path string) ([]byte, error)

	mergeTTL time.Duration

	sf   singleflight.Group
	mu   sync.Mutex
	gens map[string]*repoGen
	// stale holds the last good fetch of each serve_stale member, keyed by
	// merge key and member name.
	stale map[string]*staleEntry
}

type staleEntry struct {
	m  any
	at time.Time
}

// maxStaleAge bounds how old a serve_stale member copy may be before merges
// block on the member again and surface its error.
const maxStaleAge = time.Hour

// mergedRepo is a merged index document plus the files it references.
type mergedRepo interface {
	index() []byte
	files() map[string][]byte
}

func (r *RPMRepo) index() []byte            { return r.Repomd }
func (r *RPMRepo) files() map[string][]byte { return r.Files }
func (r *DebRepo) index() []byte            { return r.Release }
func (r *DebRepo) files() map[string][]byte { return r.Files }

// repoGen holds a virtual's current merge plus the previous one, so a client
// holding the previous index can still fetch the files it references.
type repoGen struct {
	cur, prev mergedRepo
	at, used  time.Time
}

const rpmMergeTTL = 60 * time.Second

// maxDebSuites bounds the client-chosen deb suites or alpine directories
// cached per virtual.
const maxDebSuites = 32

func NewEngine(db *database.DB, proxyEngine *proxy.Engine) *Engine {
	e := &Engine{db: db, proxyEngine: proxyEngine, getRemote: db.GetRemote, mergeTTL: rpmMergeTTL}
	e.rpmMember = e.fetchRPMMember
	e.debMember = e.fetchDebMember
	e.apkMember = e.fetchAlpineMember
	e.npmMember = e.fetchNPMMember
	e.gemsMember = e.fetchGemsMember
	e.ansibleDoc = e.ansibleMemberDoc
	e.goMember = e.fetchGoMember
	e.puppetDoc = e.fetchMemberPath
	e.memberPath = e.fetchMemberPath
	return e
}

func (e *Engine) Fetch(ctx context.Context, virt models.Virtual, path string, proxyBaseURL string) ([]byte, string, error) {
	switch virt.PackageType {
	case models.PackageRPM:
		return e.fetchRPM(ctx, virt, path)
	case models.PackageDeb:
		return e.fetchDeb(ctx, virt, path)
	case models.PackageAlpine:
		return e.fetchAlpine(ctx, virt, path)
	}

	merger, err := GetMerger(virt.PackageType)
	if err != nil {
		return nil, "", fmt.Errorf("unsupported virtual type %q: %w", virt.PackageType, err)
	}

	members, err := e.fetchMemberIndexes(ctx, virt, path)
	if err != nil {
		return nil, "", err
	}

	if len(members) == 0 {
		return nil, "", fmt.Errorf("no members reachable for virtual %q", virt.Name)
	}

	merged, err := merger.MergeIndexes(members, proxyBaseURL)
	if err != nil {
		return nil, "", fmt.Errorf("merge indexes: %w", err)
	}

	contentType := "application/octet-stream"
	switch virt.PackageType {
	case models.PackageHelm:
		contentType = "text/yaml"
	case models.PackagePyPI:
		contentType = "text/html"
	}

	return merged, contentType, nil
}

func (e *Engine) fetchMemberIndexes(ctx context.Context, virt models.Virtual, path string) ([]MemberIndex, error) {
	type result struct {
		index MemberIndex
		err   error
	}

	results := make([]result, len(virt.Members))
	var wg sync.WaitGroup

	for i, memberName := range virt.Members {
		wg.Add(1)
		go func(idx int, name string) {
			defer wg.Done()

			remote, err := e.db.GetRemote(ctx, name)
			if err != nil {
				results[idx] = result{err: fmt.Errorf("remote %q: %w", name, err)}
				return
			}

			if remote.RepoType == models.RepoTypeLocal {
				body, err := e.fetchLocalIndex(ctx, *remote, path)
				if err != nil {
					results[idx] = result{err: fmt.Errorf("local index %q: %w", name, err)}
					return
				}
				results[idx] = result{index: MemberIndex{RemoteName: name, RepoType: remote.RepoType, BaseURL: remote.BaseURL, Body: body}}
				return
			}

			prov, err := provider.Get(remote.PackageType)
			if err != nil {
				results[idx] = result{err: fmt.Errorf("provider %q: %w", remote.PackageType, err)}
				return
			}

			fetchResult, err := e.proxyEngine.Fetch(ctx, *remote, path, prov)
			if err != nil {
				results[idx] = result{err: fmt.Errorf("fetch %q/%s: %w", name, path, err)}
				return
			}
			defer func() { _ = fetchResult.Reader.Close() }()

			body, err := io.ReadAll(fetchResult.Reader)
			if err != nil {
				results[idx] = result{err: fmt.Errorf("read %q: %w", name, err)}
				return
			}

			results[idx] = result{index: MemberIndex{RemoteName: name, RepoType: remote.RepoType, BaseURL: remote.BaseURL, Body: body}}
		}(i, memberName)
	}

	wg.Wait()

	var members []MemberIndex
	for _, r := range results {
		if r.err != nil {
			slog.Warn("virtual member fetch failed", "error", r.err)
			continue
		}
		members = append(members, r.index)
	}

	return members, nil
}

func (e *Engine) fetchLocalIndex(ctx context.Context, remote models.Remote, path string) ([]byte, error) {
	prov, err := provider.Get(remote.PackageType)
	if err != nil {
		return nil, fmt.Errorf("no provider for %q: %w", remote.PackageType, err)
	}

	indexer, ok := prov.(provider.LocalIndexer)
	if !ok {
		return nil, fmt.Errorf("provider %q does not support local index generation", remote.PackageType)
	}

	return indexer.GenerateLocalIndex(ctx, e.db, remote.Name, path)
}

var (
	ErrNotFound = errors.New("not found")
	ErrBadPath  = errors.New("invalid path")
)

// MemberRedirect maps an rpm, deb, npm or ansible virtual package path
// (<member>/<href>, as written by MergeRPM/MergeDeb/MergeNPM/AnsibleVersion),
// or an alpine <dir>/<name>-<version>.apk, to the owning member's route. ok is false for any other path; ErrBadPath
// means the path is absolute or escapes the member.
func (e *Engine) MemberRedirect(ctx context.Context, virt models.Virtual, path, rawQuery, proxyBaseURL string) (string, bool, error) {
	switch virt.PackageType {
	case models.PackageRPM:
		if strings.HasPrefix(path, "repodata/") {
			return "", false, nil
		}
	case models.PackageDeb:
		// apt requests a flat repo's files under its "./" dist verbatim.
		path = strings.TrimPrefix(path, "./")
		if strings.HasPrefix(path, "dists/") {
			return "", false, nil
		}
	case models.PackageAlpine:
		return e.alpineRedirect(ctx, virt, path, rawQuery, proxyBaseURL)
	case models.PackageNPM, models.PackageAnsible:
	default:
		return "", false, nil
	}
	name, rest, found := strings.Cut(path, "/")
	if !found || !slices.Contains(virt.Members, name) {
		return "", false, nil
	}
	return e.memberLocation(ctx, name, rest, rawQuery, proxyBaseURL)
}

// memberLocation returns the URL of rest on member name's own route.
func (e *Engine) memberLocation(ctx context.Context, name, rest, rawQuery, proxyBaseURL string) (string, bool, error) {
	segs, err := memberSegments(rest)
	if err != nil {
		return "", false, err
	}
	remote, err := e.getRemote(ctx, name)
	if err != nil {
		return "", false, nil
	}
	loc := fmt.Sprintf("%s/api/v1/%s/%s/%s", strings.TrimRight(proxyBaseURL, "/"), remote.RepoType, url.PathEscape(name), strings.Join(segs, "/"))
	if rawQuery != "" {
		loc += "?" + rawQuery
	}
	return loc, true, nil
}

// memberSegments decodes rest and returns its path-escaped segments, rejecting
// absolute paths and any "." or ".." segment.
func memberSegments(rest string) ([]string, error) {
	dec, err := url.PathUnescape(rest)
	if err != nil || dec == "" || strings.HasPrefix(dec, "/") {
		return nil, ErrBadPath
	}
	segs := strings.Split(dec, "/")
	for i, s := range segs {
		if s == "." || s == ".." || strings.Contains(s, "\\") {
			return nil, ErrBadPath
		}
		segs[i] = url.PathEscape(s)
	}
	return segs, nil
}

func (e *Engine) fetchRPM(ctx context.Context, virt models.Virtual, path string) ([]byte, string, error) {
	if !strings.HasPrefix(path, "repodata/") {
		return nil, "", ErrNotFound
	}
	key := "rpm\x00" + virt.Name
	if path != "repodata/repomd.xml" {
		if body, ok := e.cachedFile(key, path); ok {
			return body, "application/gzip", nil
		}
	}

	repo, err := e.merged(ctx, key, func(ctx context.Context) (mergedRepo, error) {
		return e.mergeRPM(ctx, virt)
	})
	if err != nil {
		return nil, "", err
	}
	if path == "repodata/repomd.xml" {
		return repo.index(), "application/xml", nil
	}
	if body, ok := repo.files()[path]; ok {
		return body, "application/gzip", nil
	}
	return nil, "", ErrNotFound
}

// merged returns the merge cached under key, reusing it for mergeTTL and
// collapsing concurrent merges of the same key into one.
// ponytail: per-replica cache; behind a non-sticky LB a data request landing on
// another replica re-merges and 404s if a member changed in between. Move to the
// shared redis cache if that shows up.
func (e *Engine) merged(ctx context.Context, key string, merge func(context.Context) (mergedRepo, error)) (mergedRepo, error) {
	e.mu.Lock()
	g := e.gens[key]
	if g != nil && time.Since(g.at) < e.mergeTTL {
		g.used = time.Now()
		e.mu.Unlock()
		return g.cur, nil
	}
	e.mu.Unlock()

	v, err, _ := e.sf.Do(key, func() (any, error) {
		e.pruneStale()
		repo, err := merge(context.WithoutCancel(ctx))
		if err != nil {
			return nil, err
		}
		e.mu.Lock()
		defer e.mu.Unlock()
		if e.gens == nil {
			e.gens = map[string]*repoGen{}
		}
		g := e.gens[key]
		switch {
		case g == nil:
			e.gens[key] = &repoGen{cur: repo, at: time.Now(), used: time.Now()}
		case string(g.cur.index()) == string(repo.index()):
			g.at, g.used = time.Now(), time.Now()
			repo = g.cur
		default:
			g.prev, g.cur, g.at, g.used = g.cur, repo, time.Now(), time.Now()
		}
		return repo, nil
	})
	if err != nil {
		return nil, err
	}
	return v.(mergedRepo), nil
}

func (e *Engine) mergeRPM(ctx context.Context, virt models.Virtual) (*RPMRepo, error) {
	members, err := fetchMembers(virt, func(name string) (*RPMMember, error) {
		return staleMember(ctx, e, "rpm\x00"+virt.Name, name, func(ctx context.Context) (*RPMMember, error) { return e.rpmMember(ctx, name, virt.ParallelFetch) })
	})
	if err != nil {
		return nil, err
	}
	repo, err := MergeRPM(members)
	if err != nil {
		return nil, fmt.Errorf("merge rpm repodata: %w", err)
	}
	return repo, nil
}

// fetchMembers reads every member in parallel, failing if any member fails.
func fetchMembers[T any](virt models.Virtual, fetch func(name string) (*T, error)) ([]T, error) {
	members := make([]T, len(virt.Members))
	errs := make([]error, len(virt.Members))
	var wg sync.WaitGroup
	for i, name := range virt.Members {
		wg.Add(1)
		go func() {
			defer wg.Done()
			m, err := fetch(name)
			if err != nil {
				errs[i] = fmt.Errorf("member %q: %w", name, err)
				return
			}
			members[i] = *m
		}()
	}
	wg.Wait()
	// %v, not %w: a member failure is a 502 whatever its cause, never a 404.
	if err := errors.Join(errs...); err != nil {
		return nil, fmt.Errorf("virtual %q: %v", virt.Name, err)
	}
	return members, nil
}

func (e *Engine) fetchDeb(ctx context.Context, virt models.Virtual, reqPath string) ([]byte, string, error) {
	base, name, ok := debIndexPath(strings.TrimPrefix(path.Clean("/"+reqPath), "/"))
	if !ok {
		return nil, "", ErrNotFound
	}
	key := "deb\x00" + virt.Name + "\x00" + base
	if name != "Release" {
		if body, ok := e.cachedFile(key, name); ok {
			return body, debContentType(name), nil
		}
	}

	repo, err := e.merged(ctx, key, func(ctx context.Context) (mergedRepo, error) {
		members, err := fetchMembers(virt, func(name string) (*DebMember, error) {
			return staleMember(ctx, e, key, name, func(ctx context.Context) (*DebMember, error) { return e.debMember(ctx, name, base, virt.ParallelFetch) })
		})
		if err != nil {
			return nil, err
		}
		return MergeDeb(members), nil
	})
	if err != nil {
		return nil, "", err
	}
	e.evictLRU("deb\x00"+virt.Name+"\x00", maxDebSuites)
	if name == "Release" {
		return repo.index(), "text/plain", nil
	}
	if body, ok := repo.files()[name]; ok {
		return body, debContentType(name), nil
	}
	return nil, "", ErrNotFound
}

func debContentType(name string) string {
	if strings.HasSuffix(name, ".gz") {
		return "application/gzip"
	}
	return "text/plain"
}

// fetchDebMember reads a member's Release under base and each binary Packages
// index it lists.
func (e *Engine) fetchDebMember(ctx context.Context, name, base string, parallel bool) (*DebMember, error) {
	remote, err := e.getRemote(ctx, name)
	if err != nil {
		return nil, fmt.Errorf("remote %q: %w", name, err)
	}
	rel, err := e.memberPath(ctx, *remote, base+"Release")
	if err != nil {
		return nil, err
	}
	pkgs, err := fetchFiles(ctx, parallel, debIndexFiles(parseDebRelease(rel)), func(ctx context.Context, file string) ([]byte, error) {
		return e.fetchDecompressed(ctx, *remote, base, file)
	})
	if err != nil {
		return nil, err
	}
	return &DebMember{RemoteName: name, Release: rel, Packages: pkgs}, nil
}

func (e *Engine) mergedAlpine(ctx context.Context, virt models.Virtual, dir string) (mergedRepo, error) {
	key := "alpine\x00" + virt.Name + "\x00" + dir
	repo, err := e.merged(ctx, key, func(ctx context.Context) (mergedRepo, error) {
		members, err := fetchMembers(virt, func(name string) (*AlpineMember, error) {
			return staleMember(ctx, e, key, name, func(ctx context.Context) (*AlpineMember, error) { return e.apkMember(ctx, name, dir) })
		})
		if err != nil {
			return nil, err
		}
		return MergeAlpine(members), nil
	})
	if err == nil {
		e.evictLRU("alpine\x00"+virt.Name+"\x00", maxDebSuites)
	}
	return repo, err
}

func (e *Engine) fetchAlpine(ctx context.Context, virt models.Virtual, reqPath string) ([]byte, string, error) {
	dir, file, ok := alpinePath(strings.TrimPrefix(path.Clean("/"+reqPath), "/"))
	if !ok {
		return nil, "", ErrNotFound
	}
	repo, err := e.mergedAlpine(ctx, virt, dir)
	if err != nil {
		return nil, "", err
	}
	if file != "APKINDEX.tar.gz" {
		return nil, "", ErrNotFound
	}
	return repo.index(), "application/gzip", nil
}

// alpineRedirect finds the member whose index lists the requested .apk; apk
// builds the download path itself, so the index cannot carry a member prefix.
// A failed merge returns ok=false so Fetch re-merges and fails with 502.
func (e *Engine) alpineRedirect(ctx context.Context, virt models.Virtual, reqPath, rawQuery, proxyBaseURL string) (string, bool, error) {
	dir, file, ok := alpinePath(strings.TrimPrefix(path.Clean("/"+reqPath), "/"))
	if !ok || file == "APKINDEX.tar.gz" {
		return "", false, nil
	}
	owner, ok := e.cachedFile("alpine\x00"+virt.Name+"\x00"+dir, file)
	if !ok {
		repo, err := e.mergedAlpine(ctx, virt, dir)
		if err != nil {
			return "", false, nil
		}
		if owner, ok = repo.files()[file]; !ok {
			return "", false, nil
		}
	}
	return e.memberLocation(ctx, string(owner), dir+file, rawQuery, proxyBaseURL)
}

// fetchAlpineMember reads a member's APKINDEX under dir; a member without that
// directory contributes nothing.
func (e *Engine) fetchAlpineMember(ctx context.Context, name, dir string) (*AlpineMember, error) {
	remote, err := e.getRemote(ctx, name)
	if err != nil {
		return nil, fmt.Errorf("remote %q: %w", name, err)
	}
	raw, err := e.fetchMemberPath(ctx, *remote, dir+"APKINDEX.tar.gz")
	var pe *proxy.ProxyError
	if errors.Is(err, ErrNotFound) || errors.As(err, &pe) && pe.Status == http.StatusNotFound {
		return &AlpineMember{RemoteName: name}, nil
	}
	if err != nil {
		return nil, err
	}
	idx, err := apkIndexText(raw)
	if err != nil {
		return nil, fmt.Errorf("%s/%sAPKINDEX.tar.gz: %w", name, dir, err)
	}
	return &AlpineMember{RemoteName: name, Index: idx}, nil
}

// staleMember fetches member name for the merge under key. A serve_stale member
// with a good fetch younger than maxStaleAge returns it at once and refreshes in
// the background, one refresh per member at a time; the next merge picks it up.
func staleMember[T any](ctx context.Context, e *Engine, key, name string, fetch func(context.Context) (*T, error)) (*T, error) {
	sk := key + "\x00" + name
	remote, lookupErr := e.getRemote(ctx, name)
	if lookupErr == nil && !remote.ServeStale {
		e.mu.Lock()
		delete(e.stale, sk)
		e.mu.Unlock()
		return fetch(ctx)
	}
	e.mu.Lock()
	if e.stale == nil {
		e.stale = map[string]*staleEntry{}
	}
	prev := e.stale[sk]
	// A failed lookup serves an existing copy but never creates one.
	if lookupErr != nil && (prev == nil || prev.m == nil) {
		e.mu.Unlock()
		return fetch(ctx)
	}
	// A cold fetch holds a placeholder so eviction or toggle-off during it is seen.
	token := prev
	if prev == nil || prev.m == nil {
		prev, token = nil, &staleEntry{at: time.Now()}
		e.stale[sk] = token
	}
	e.mu.Unlock()
	refresh := func() (any, error) {
		m, err := fetch(ctx)
		e.mu.Lock()
		// An evicted or replaced entry means this refresh is no longer wanted.
		if e.stale[sk] == token {
			if err == nil {
				e.stale[sk] = &staleEntry{m: m, at: time.Now()}
			} else if prev == nil {
				delete(e.stale, sk)
			}
		}
		e.mu.Unlock()
		return m, err
	}
	if prev == nil {
		m, err := refresh()
		return m.(*T), err
	}
	ch := e.sf.DoChan("stale\x00"+sk, refresh)
	go func() {
		if r := <-ch; r.Err != nil {
			slog.Warn("virtual member background refresh failed", "member", name, "error", r.Err)
		}
	}()
	return prev.m.(*T), nil
}

// pruneStale drops serve_stale copies past maxStaleAge, including those of
// removed members and virtuals.
func (e *Engine) pruneStale() {
	e.mu.Lock()
	defer e.mu.Unlock()
	for k, se := range e.stale {
		if time.Since(se.at) >= maxStaleAge {
			delete(e.stale, k)
		}
	}
}

// evictLRU drops the least recently used merges under prefix beyond keep.
func (e *Engine) evictLRU(prefix string, keep int) {
	e.mu.Lock()
	defer e.mu.Unlock()
	var keys []string
	for k := range e.gens {
		if strings.HasPrefix(k, prefix) {
			keys = append(keys, k)
		}
	}
	slices.SortFunc(keys, func(a, b string) int { return e.gens[b].used.Compare(e.gens[a].used) })
	for _, k := range keys[min(len(keys), keep):] {
		delete(e.gens, k)
		for sk := range e.stale {
			if strings.HasPrefix(sk, k+"\x00") {
				delete(e.stale, sk)
			}
		}
	}
}

func (e *Engine) cachedFile(key, path string) ([]byte, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	g := e.gens[key]
	if g == nil {
		return nil, false
	}
	for _, r := range []mergedRepo{g.cur, g.prev} {
		if r == nil {
			continue
		}
		if body, ok := r.files()[path]; ok {
			g.used = time.Now()
			return body, true
		}
	}
	return nil, false
}

func (e *Engine) fetchRPMMember(ctx context.Context, name string, parallel bool) (*RPMMember, error) {
	remote, err := e.getRemote(ctx, name)
	if err != nil {
		return nil, fmt.Errorf("remote %q: %w", name, err)
	}
	repomd, err := e.memberPath(ctx, *remote, "repodata/repomd.xml")
	if err != nil {
		return nil, err
	}
	locs, ts, err := parseRepomd(repomd)
	if err != nil {
		return nil, err
	}
	data, err := fetchFiles(ctx, parallel, locs, func(ctx context.Context, href string) ([]byte, error) {
		return e.fetchDecompressed(ctx, *remote, "", href)
	})
	if err != nil {
		return nil, err
	}
	return &RPMMember{RemoteName: name, Bases: remote.UpstreamPool(), Timestamp: ts, Data: data}, nil
}

func (e *Engine) fetchDecompressed(ctx context.Context, remote models.Remote, base, file string) ([]byte, error) {
	raw, err := e.memberPath(ctx, remote, base+file)
	if err != nil {
		return nil, err
	}
	body, err := decompress(file, raw)
	if err != nil {
		return nil, fmt.Errorf("decompress %s: %w", file, err)
	}
	return body, nil
}

// maxFileFetches bounds the concurrent index file fetches per member.
const maxFileFetches = 4

// fetchFiles fetches each key's file, one at a time or up to maxFileFetches at
// once, failing on the first error and cancelling the rest.
func fetchFiles(ctx context.Context, parallel bool, files map[string]string, fetch func(ctx context.Context, file string) ([]byte, error)) (map[string][]byte, error) {
	out := make(map[string][]byte, len(files))
	if !parallel {
		for k, f := range files {
			body, err := fetch(ctx, f)
			if err != nil {
				return nil, err
			}
			out[k] = body
		}
		return out, nil
	}
	var mu sync.Mutex
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(maxFileFetches)
	for k, f := range files {
		g.Go(func() error {
			if err := gctx.Err(); err != nil {
				return err
			}
			body, err := fetch(gctx, f)
			if err != nil {
				return err
			}
			mu.Lock()
			out[k] = body
			mu.Unlock()
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return nil, err
	}
	return out, nil
}

// fetchMemberPath reads one path from a member the same way its own route
// would serve it: local index, synthesized remote (e.g. github_rpm), or proxy.
func (e *Engine) fetchMemberPath(ctx context.Context, remote models.Remote, path string) ([]byte, error) {
	prov, err := provider.Get(remote.PackageType)
	if err != nil {
		return nil, fmt.Errorf("provider %q: %w", remote.PackageType, err)
	}

	var serve func(w http.ResponseWriter, r *http.Request) bool
	if remote.RepoType == models.RepoTypeLocal {
		indexer, ok := prov.(provider.LocalIndexer)
		if !ok {
			return nil, fmt.Errorf("provider %q does not serve a local index", remote.PackageType)
		}
		reqPath, _, _ := strings.Cut(path, "?")
		serve = func(w http.ResponseWriter, r *http.Request) bool {
			return indexer.ServeLocalIndex(w, r, e.db, remote.Name, reqPath)
		}
	} else if rs, ok := prov.(provider.RemoteServer); ok {
		serve = func(w http.ResponseWriter, r *http.Request) bool {
			return rs.ServeRemote(w, r, remote, path, "", e.db)
		}
	}

	if serve != nil {
		rec := httptest.NewRecorder()
		if serve(rec, httptest.NewRequestWithContext(ctx, http.MethodGet, "/"+path, nil)) {
			if rec.Code == http.StatusNotFound || rec.Code == http.StatusGone {
				return nil, fmt.Errorf("%s/%s: %w", remote.Name, path, ErrNotFound)
			}
			if rec.Code != http.StatusOK {
				return nil, fmt.Errorf("%s/%s: status %d", remote.Name, path, rec.Code)
			}
			return rec.Body.Bytes(), nil
		}
		if remote.RepoType == models.RepoTypeLocal {
			return nil, fmt.Errorf("%s/%s: %w", remote.Name, path, ErrNotFound)
		}
	}

	res, err := e.proxyEngine.Fetch(ctx, remote, path, prov)
	if err != nil {
		return nil, fmt.Errorf("fetch %s/%s: %w", remote.Name, path, err)
	}
	defer func() { _ = res.Reader.Close() }()
	return io.ReadAll(res.Reader)
}
