// Package vcs caches source archives and release assets from GitHub or Gitea,
// keyed by ref: tags, commits and release assets are immutable, and branch
// archives redirect to the archive of the commit the branch currently points at.
package vcs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"

	"github.com/IUCLabs/artifactapi/internal/provider"
	"github.com/IUCLabs/artifactapi/internal/provider/generic"
	"github.com/IUCLabs/artifactapi/internal/proxy"
	"github.com/IUCLabs/artifactapi/pkg/models"
)

func init() {
	provider.Register(New())
}

type kind int

const (
	tagArchive kind = iota + 1
	branchArchive
	commitArchive
	releaseAsset
	latestReleaseAsset
)

// target is a parsed vcs request path.
type target struct {
	Kind        kind
	Owner, Repo string
	Name        string // tag, branch, commit or asset
	Ext         string
}

var (
	segmentRe = regexp.MustCompile(`^[A-Za-z0-9._+-]+$`)
	commitRe  = regexp.MustCompile(`^[0-9a-f]{40}$`)
)

var errNotFound = errors.New("ref not found")

const (
	maxRefs     = 4096
	negativeTTL = time.Minute
)

// Provider embeds generic for upstream URLs, content types and GitHub/Basic auth.
type Provider struct {
	generic.Provider

	client    *http.Client
	githubAPI string

	group singleflight.Group
	mu    sync.Mutex
	refs  map[string]map[string]resolved
}

// resolved is a branch lookup; an empty commit records a missing branch.
type resolved struct {
	commit string
	at     time.Time
}

func New() *Provider {
	return &Provider{
		client:    &http.Client{Timeout: 30 * time.Second},
		githubAPI: "https://api.github.com",
		refs:      map[string]map[string]resolved{},
	}
}

func (p *Provider) Type() models.PackageType { return models.PackageVCS }

func (p *Provider) Classify(path string) provider.Mutability {
	if ref, ok := parsePath(path); ok && (ref.Kind == branchArchive || ref.Kind == latestReleaseAsset) {
		return provider.Mutable
	}
	return provider.Immutable
}

// parsePath accepts only the archive and release-download shapes, rejecting
// any segment that could escape owner/repo or a ref.
func parsePath(path string) (target, bool) {
	parts := strings.SplitN(strings.TrimPrefix(path, "/"), "/", 3)
	if len(parts) != 3 || !validSegment(parts[0]) || !validSegment(parts[1]) {
		return target{}, false
	}
	ref := target{Owner: parts[0], Repo: parts[1]}
	rest := parts[2]
	switch {
	case strings.HasPrefix(rest, "archive/"):
		name, ext, ok := archiveName(strings.TrimPrefix(rest, "archive/"))
		if !ok {
			return target{}, false
		}
		ref.Ext = ext
		switch {
		case strings.HasPrefix(name, "refs/tags/"):
			ref.Kind, ref.Name = tagArchive, strings.TrimPrefix(name, "refs/tags/")
		case strings.HasPrefix(name, "refs/heads/"):
			ref.Kind, ref.Name = branchArchive, strings.TrimPrefix(name, "refs/heads/")
		case commitRe.MatchString(name):
			return target{Kind: commitArchive, Owner: ref.Owner, Repo: ref.Repo, Name: name, Ext: ext}, true
		default:
			return target{}, false
		}
		return ref, validRefName(ref.Name)
	case strings.HasPrefix(rest, "releases/latest/download/"):
		ref.Kind, ref.Name = latestReleaseAsset, strings.TrimPrefix(rest, "releases/latest/download/")
		return ref, validSegment(ref.Name)
	case strings.HasPrefix(rest, "releases/download/"):
		tagAsset := strings.TrimPrefix(rest, "releases/download/")
		i := strings.LastIndex(tagAsset, "/")
		if i < 0 {
			return target{}, false
		}
		ref.Kind, ref.Name = releaseAsset, tagAsset[i+1:]
		return ref, validRefName(tagAsset[:i]) && validSegment(ref.Name)
	}
	return target{}, false
}

func archiveName(s string) (string, string, bool) {
	for _, ext := range []string{".tar.gz", ".zip"} {
		if name, ok := strings.CutSuffix(s, ext); ok && name != "" {
			return name, ext, true
		}
	}
	return "", "", false
}

func validSegment(s string) bool {
	return segmentRe.MatchString(s) && s != "." && s != ".." && !strings.Contains(s, "..")
}

func validRefName(s string) bool {
	for _, seg := range strings.Split(s, "/") {
		if !validSegment(seg) || strings.HasPrefix(seg, ".") {
			return false
		}
	}
	return true
}

// ServeRemote rejects unsupported paths and redirects branch archives to the
// immutable archive of the branch's current commit; everything else is proxied.
func (p *Provider) ServeRemote(w http.ResponseWriter, r *http.Request, remote models.Remote, path, proxyBaseURL string, _ provider.RemoteMetadataStore) bool {
	ref, ok := parsePath(path)
	if !ok {
		http.Error(w, "unsupported vcs path", http.StatusNotFound)
		return true
	}
	if ref.Kind != branchArchive {
		return false
	}
	classifier := proxy.NewClassifier(p)
	if classifier.Classify(remote, path) == proxy.ClassDenied {
		http.Error(w, "access denied", http.StatusForbidden)
		return true
	}
	commit, err := p.resolve(r.Context(), remote, ref)
	var se *provider.UpstreamStatusError
	switch {
	case errors.Is(err, errNotFound):
		http.Error(w, "branch not found", http.StatusNotFound)
		return true
	case errors.As(err, &se) && (se.Status == http.StatusTooManyRequests || se.Status == http.StatusForbidden && !se.RetryAt.IsZero()):
		secs := 60
		if !se.RetryAt.IsZero() {
			secs = max(1, int(time.Until(se.RetryAt).Seconds()+1))
		}
		w.Header().Set("Retry-After", strconv.Itoa(secs))
		http.Error(w, "upstream rate limited", http.StatusServiceUnavailable)
		return true
	case err != nil:
		http.Error(w, "bad gateway", http.StatusBadGateway)
		return true
	}
	target := ref.Owner + "/" + ref.Repo + "/archive/" + commit + ref.Ext
	if classifier.Classify(remote, target) == proxy.ClassDenied {
		http.Error(w, "access denied", http.StatusForbidden)
		return true
	}
	http.Redirect(w, r, strings.TrimRight(proxyBaseURL, "/")+provider.RemoteMountPath+remote.Name+"/"+target, http.StatusFound)
	return true
}

// resolve maps a branch to its commit, reusing the answer for mutable_ttl (a
// missing branch for at least negativeTTL) and, with stale_on_error, past an
// upstream failure. Concurrent lookups of one branch share a single API call.
func (p *Provider) resolve(ctx context.Context, remote models.Remote, ref target) (string, error) {
	key := ref.Owner + "/" + ref.Repo + "\x00" + ref.Name
	ttl := time.Duration(remote.MutableTTL) * time.Second
	p.mu.Lock()
	prev, ok := p.refs[remote.Name][key]
	p.mu.Unlock()
	if ok && prev.commit != "" && time.Since(prev.at) < ttl {
		return prev.commit, nil
	}
	if ok && prev.commit == "" && time.Since(prev.at) < max(ttl, negativeTTL) {
		return "", errNotFound
	}
	v, err, _ := p.group.Do(remote.Name+"\x00"+key, func() (any, error) {
		commit, err := p.lookup(context.WithoutCancel(ctx), remote, ref)
		if err == nil || errors.Is(err, errNotFound) {
			p.store(remote.Name, key, commit)
		}
		return commit, err
	})
	if err != nil {
		if ok && prev.commit != "" && remote.StaleOnError && !errors.Is(err, errNotFound) {
			return prev.commit, nil
		}
		return "", err
	}
	return v.(string), nil
}

// store records a lookup, evicting the remote's oldest entry once it holds maxRefs.
func (p *Provider) store(remote, key, commit string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	m := p.refs[remote]
	if m == nil {
		m = map[string]resolved{}
		p.refs[remote] = m
	}
	if _, ok := m[key]; !ok && len(m) >= maxRefs {
		var oldest string
		for k, e := range m {
			if oldest == "" || e.at.Before(m[oldest].at) {
				oldest = k
			}
		}
		delete(m, oldest)
	}
	m[key] = resolved{commit: commit, at: time.Now()}
}

func (p *Provider) lookup(ctx context.Context, remote models.Remote, ref target) (string, error) {
	segs := strings.Split(ref.Name, "/")
	for i, s := range segs {
		segs[i] = url.PathEscape(s)
	}
	branch := strings.Join(segs, "/")
	github := generic.IsGitHubHost(remote.BaseURL)
	api := strings.TrimRight(remote.BaseURL, "/") + "/api/v1/repos/" + ref.Owner + "/" + ref.Repo + "/branches/" + branch
	if github {
		api = p.githubAPI + "/repos/" + ref.Owner + "/" + ref.Repo + "/git/ref/heads/" + branch
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, api, nil)
	if err != nil {
		return "", err
	}
	h, err := p.AuthHeaders(ctx, remote)
	if err != nil {
		return "", err
	}
	req.Header = h
	if github {
		req.Header.Set("Accept", "application/vnd.github+json")
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusUnprocessableEntity {
		return "", errNotFound
	}
	if resp.StatusCode != http.StatusOK {
		return "", provider.NewUpstreamStatusError(api, resp)
	}
	var b struct {
		Commit struct {
			ID string `json:"id"`
		} `json:"commit"`
		Object struct {
			SHA string `json:"sha"`
		} `json:"object"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&b); err != nil {
		return "", err
	}
	commit := b.Commit.ID
	if github {
		commit = b.Object.SHA
	}
	if !commitRe.MatchString(commit) {
		return "", fmt.Errorf("branch lookup %s: unexpected commit %q", api, commit)
	}
	return commit, nil
}
