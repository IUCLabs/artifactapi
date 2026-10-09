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
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/IUCLabs/artifactapi/internal/database"
	"github.com/IUCLabs/artifactapi/internal/provider"
	"github.com/IUCLabs/artifactapi/internal/proxy"
	"github.com/IUCLabs/artifactapi/pkg/models"
	"golang.org/x/sync/singleflight"
)

type Engine struct {
	db          *database.DB
	proxyEngine *proxy.Engine
	getRemote   func(context.Context, string) (*models.Remote, error)
	rpmMember   func(context.Context, string) (*RPMMember, error)

	mergeTTL time.Duration

	sf  singleflight.Group
	mu  sync.Mutex
	rpm map[string]*rpmGen
}

// rpmGen holds a virtual's current merge plus the previous one, so a client
// holding the previous repomd can still fetch its content-hashed data files.
type rpmGen struct {
	cur, prev *RPMRepo
	at        time.Time
}

const rpmMergeTTL = 60 * time.Second

func NewEngine(db *database.DB, proxyEngine *proxy.Engine) *Engine {
	e := &Engine{db: db, proxyEngine: proxyEngine, getRemote: db.GetRemote, mergeTTL: rpmMergeTTL}
	e.rpmMember = e.fetchRPMMember
	return e
}

func (e *Engine) Fetch(ctx context.Context, virt models.Virtual, path string, proxyBaseURL string) ([]byte, string, error) {
	if virt.PackageType == models.PackageRPM {
		return e.fetchRPM(ctx, virt, path)
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
			defer fetchResult.Reader.Close()

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

// MemberRedirect maps an rpm virtual package path (<member>/<href>, as written
// by MergeRPM) to the owning member's route. ok is false for any other path;
// ErrBadPath means the path is absolute or escapes the member.
func (e *Engine) MemberRedirect(ctx context.Context, virt models.Virtual, path, rawQuery, proxyBaseURL string) (string, bool, error) {
	if virt.PackageType != models.PackageRPM || strings.HasPrefix(path, "repodata/") {
		return "", false, nil
	}
	name, rest, found := strings.Cut(path, "/")
	if !found || !slices.Contains(virt.Members, name) {
		return "", false, nil
	}
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
	if path != "repodata/repomd.xml" {
		if body, ok := e.cachedRPMFile(virt.Name, path); ok {
			return body, "application/gzip", nil
		}
	}

	repo, err := e.mergedRPM(ctx, virt)
	if err != nil {
		return nil, "", err
	}
	if path == "repodata/repomd.xml" {
		return repo.Repomd, "application/xml", nil
	}
	if body, ok := repo.Files[path]; ok {
		return body, "application/gzip", nil
	}
	return nil, "", ErrNotFound
}

// mergedRPM returns the virtual's merge, reusing it for mergeTTL and
// collapsing concurrent merges of the same virtual into one.
// ponytail: per-replica cache; behind a non-sticky LB a data request landing on
// another replica re-merges and 404s if a member changed in between. Move to the
// shared redis cache if that shows up.
func (e *Engine) mergedRPM(ctx context.Context, virt models.Virtual) (*RPMRepo, error) {
	e.mu.Lock()
	g := e.rpm[virt.Name]
	if g != nil && time.Since(g.at) < e.mergeTTL {
		e.mu.Unlock()
		return g.cur, nil
	}
	e.mu.Unlock()

	v, err, _ := e.sf.Do(virt.Name, func() (any, error) {
		repo, err := e.mergeRPM(context.WithoutCancel(ctx), virt)
		if err != nil {
			return nil, err
		}
		e.mu.Lock()
		defer e.mu.Unlock()
		if e.rpm == nil {
			e.rpm = map[string]*rpmGen{}
		}
		g := e.rpm[virt.Name]
		switch {
		case g == nil:
			e.rpm[virt.Name] = &rpmGen{cur: repo, at: time.Now()}
		case string(g.cur.Repomd) == string(repo.Repomd):
			g.at = time.Now()
			repo = g.cur
		default:
			g.prev, g.cur, g.at = g.cur, repo, time.Now()
		}
		return repo, nil
	})
	if err != nil {
		return nil, err
	}
	return v.(*RPMRepo), nil
}

func (e *Engine) mergeRPM(ctx context.Context, virt models.Virtual) (*RPMRepo, error) {
	members := make([]RPMMember, len(virt.Members))
	errs := make([]error, len(virt.Members))
	var wg sync.WaitGroup
	for i, name := range virt.Members {
		wg.Add(1)
		go func() {
			defer wg.Done()
			m, err := e.rpmMember(ctx, name)
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

	repo, err := MergeRPM(members)
	if err != nil {
		return nil, fmt.Errorf("merge rpm repodata: %w", err)
	}
	return repo, nil
}

func (e *Engine) cachedRPMFile(virt, path string) ([]byte, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	g := e.rpm[virt]
	if g == nil {
		return nil, false
	}
	for _, r := range []*RPMRepo{g.cur, g.prev} {
		if r == nil {
			continue
		}
		if body, ok := r.Files[path]; ok {
			return body, true
		}
	}
	return nil, false
}

func (e *Engine) fetchRPMMember(ctx context.Context, name string) (*RPMMember, error) {
	remote, err := e.getRemote(ctx, name)
	if err != nil {
		return nil, fmt.Errorf("remote %q: %w", name, err)
	}
	repomd, err := e.fetchMemberPath(ctx, *remote, "repodata/repomd.xml")
	if err != nil {
		return nil, err
	}
	locs, ts, err := parseRepomd(repomd)
	if err != nil {
		return nil, err
	}
	m := &RPMMember{RemoteName: name, Bases: remote.UpstreamPool(), Timestamp: ts, Data: map[string][]byte{}}
	for t, href := range locs {
		raw, err := e.fetchMemberPath(ctx, *remote, href)
		if err != nil {
			return nil, err
		}
		if m.Data[t], err = decompress(href, raw); err != nil {
			return nil, fmt.Errorf("decompress %s: %w", href, err)
		}
	}
	return m, nil
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
		serve = func(w http.ResponseWriter, r *http.Request) bool {
			return indexer.ServeLocalIndex(w, r, e.db, remote.Name, path)
		}
	} else if rs, ok := prov.(provider.RemoteServer); ok {
		serve = func(w http.ResponseWriter, r *http.Request) bool {
			return rs.ServeRemote(w, r, remote, path, "", e.db)
		}
	}

	if serve != nil {
		rec := httptest.NewRecorder()
		if serve(rec, httptest.NewRequestWithContext(ctx, http.MethodGet, "/"+path, nil)) {
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
	defer res.Reader.Close()
	return io.ReadAll(res.Reader)
}
