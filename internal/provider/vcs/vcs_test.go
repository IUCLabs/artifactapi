package vcs

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/IUCLabs/artifactapi/internal/githubauth"
	"github.com/IUCLabs/artifactapi/internal/provider"
	"github.com/IUCLabs/artifactapi/pkg/models"
)

const (
	shaA = "4f2cfe92e3499f8581bf39db77e0a1431fa7b55c"
	shaB = "7fa5434dbc083c853eef435d1915020345219d2f"
)

func TestParsePath(t *testing.T) {
	tests := []struct {
		path string
		ok   bool
		kind kind
		name string
	}{
		{"cli/cli/archive/refs/tags/v2.60.0.tar.gz", true, tagArchive, "v2.60.0"},
		{"/cli/cli/archive/refs/tags/v2.60.0.zip", true, tagArchive, "v2.60.0"},
		{"o/r/archive/refs/tags/release/1.0.tar.gz", true, tagArchive, "release/1.0"},
		{"o/r/archive/refs/heads/feature/x.tar.gz", true, branchArchive, "feature/x"},
		{"o/r/archive/" + shaA + ".tar.gz", true, commitArchive, shaA},
		{"o/r/releases/download/v1.0/tool_linux_amd64.tar.gz", true, releaseAsset, "tool_linux_amd64.tar.gz"},
		{"o/r/releases/latest/download/tool.zip", true, latestReleaseAsset, "tool.zip"},
		{"o/r/archive/main.tar.gz", false, 0, ""},
		{"o/r/archive/" + shaA[:7] + ".tar.gz", false, 0, ""},
		{"o/r/archive/refs/tags/v1.tar.bz2", false, 0, ""},
		{"o/r/archive/refs/tags/.tar.gz", false, 0, ""},
		{"../r/archive/refs/tags/v1.tar.gz", false, 0, ""},
		{"o/../archive/refs/tags/v1.tar.gz", false, 0, ""},
		{"o/r/archive/refs/tags/../../../etc/passwd.tar.gz", false, 0, ""},
		{"o/r/archive/refs/heads/a//b.tar.gz", false, 0, ""},
		{"o/r/archive/refs/heads/.hidden.tar.gz", false, 0, ""},
		{"o/r/archive/refs/heads/a..b.tar.gz", false, 0, ""},
		{"o/r/archive/refs/tags/v1%2f.tar.gz", false, 0, ""},
		{"o/r/releases/download/v1/../x", false, 0, ""},
		{"o/r/releases/download/asset", false, 0, ""},
		{"o/r/raw/main/README.md", false, 0, ""},
		{"o/r", false, 0, ""},
	}
	for _, tt := range tests {
		ref, ok := parsePath(tt.path)
		if ok != tt.ok || (ok && (ref.Kind != tt.kind || ref.Name != tt.name)) {
			t.Errorf("parsePath(%q) = %+v, %v; want kind %v name %q ok %v", tt.path, ref, ok, tt.kind, tt.name, tt.ok)
		}
	}
}

func TestClassify(t *testing.T) {
	p := New()
	for path, want := range map[string]provider.Mutability{
		"o/r/archive/refs/tags/v1.tar.gz":        provider.Immutable,
		"o/r/archive/" + shaA + ".zip":           provider.Immutable,
		"o/r/releases/download/v1/a.bin":         provider.Immutable,
		"o/r/archive/refs/heads/main.tar.gz":     provider.Mutable,
		"o/r/releases/latest/download/webui.zip": provider.Mutable,
	} {
		if got := p.Classify(path); got != want {
			t.Errorf("Classify(%q) = %v, want %v", path, got, want)
		}
	}
}

type upstream struct {
	srv    *httptest.Server
	hits   atomic.Int32
	commit atomic.Value
	status atomic.Int32
	header atomic.Value
	accept atomic.Value
	auth   atomic.Value
	path   atomic.Value
	gate   chan struct{}
}

func newUpstream(t *testing.T, gitea bool) *upstream {
	u := &upstream{}
	u.commit.Store(shaA)
	u.status.Store(http.StatusOK)
	u.header.Store(http.Header{})
	u.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u.hits.Add(1)
		if u.gate != nil {
			<-u.gate
		}
		u.accept.Store(r.Header.Get("Accept"))
		u.auth.Store(r.Header.Get("Authorization"))
		u.path.Store(r.URL.EscapedPath())
		for k, v := range u.header.Load().(http.Header) {
			w.Header()[k] = v
		}
		if s := int(u.status.Load()); s != http.StatusOK {
			w.WriteHeader(s)
			return
		}
		if gitea {
			_, _ = w.Write([]byte(`{"name":"main","commit":{"id":"` + u.commit.Load().(string) + `"}}`))
			return
		}
		_, _ = w.Write([]byte(`{"ref":"refs/heads/main","object":{"sha":"` + u.commit.Load().(string) + `","type":"commit"}}`))
	}))
	t.Cleanup(u.srv.Close)
	return u
}

func serve(p *Provider, remote models.Remote, path string) (*httptest.ResponseRecorder, bool) {
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/api/v1/remote/"+remote.Name+"/"+path, nil)
	return w, p.ServeRemote(w, r, remote, path, "http://proxy", nil)
}

// age backdates every cached lookup of a remote by d.
func age(p *Provider, remote string, d time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for k, e := range p.refs[remote] {
		e.at = e.at.Add(-d)
		p.refs[remote][k] = e
	}
}

type staticCred string

func (s staticCred) Token(context.Context) (string, error) { return string(s), nil }

func TestServeRemoteGiteaBranchRedirectsToCommit(t *testing.T) {
	u := newUpstream(t, true)
	p := New()
	remote := models.Remote{Name: "gitea", BaseURL: u.srv.URL, MutableTTL: 3600, Username: "bot", Password: "pw"}

	w, handled := serve(p, remote, "myorg/app/archive/refs/heads/feature/x.zip")
	if !handled || w.Code != http.StatusFound {
		t.Fatalf("handled=%v code=%d", handled, w.Code)
	}
	if got, want := w.Header().Get("Location"), "http://proxy/api/v1/remote/gitea/myorg/app/archive/"+shaA+".zip"; got != want {
		t.Errorf("Location = %q, want %q", got, want)
	}
	if got := u.path.Load(); got != "/api/v1/repos/myorg/app/branches/feature/x" {
		t.Errorf("api path = %q", got)
	}
	if got := u.auth.Load(); got != "Basic Ym90OnB3" {
		t.Errorf("Authorization = %q, want remote basic auth", got)
	}
}

func TestServeRemoteGitHubBranchUsesAPI(t *testing.T) {
	githubauth.SetServer(staticCred("ghs_server"))
	t.Cleanup(func() { githubauth.SetServer(nil) })
	u := newUpstream(t, false)
	p := New()
	p.githubAPI = u.srv.URL

	for _, base := range []string{"https://github.com", "https://www.github.com/", "https://api.github.com"} {
		remote := models.Remote{Name: base, BaseURL: base, MutableTTL: 3600}
		w, _ := serve(p, remote, "cli/cli/archive/refs/heads/feature/x.tar.gz")
		if !strings.HasSuffix(w.Header().Get("Location"), "/cli/cli/archive/"+shaA+".tar.gz") {
			t.Fatalf("%s: Location = %q", base, w.Header().Get("Location"))
		}
		if u.path.Load() != "/repos/cli/cli/git/ref/heads/feature/x" || u.accept.Load() != "application/vnd.github+json" {
			t.Errorf("%s: api path %q accept %q", base, u.path.Load(), u.accept.Load())
		}
		if got := u.auth.Load(); got != "Bearer ghs_server" {
			t.Errorf("%s: Authorization = %q, want server credential", base, got)
		}
	}
}

func TestServeRemoteBranchResolutionHonoursMutableTTL(t *testing.T) {
	u := newUpstream(t, true)
	p := New()
	remote := models.Remote{Name: "g", BaseURL: u.srv.URL, MutableTTL: 600}
	path := "o/r/archive/refs/heads/main.tar.gz"

	serve(p, remote, path)
	u.commit.Store(shaB)
	age(p, remote.Name, 599*time.Second)
	w, _ := serve(p, remote, path)
	if u.hits.Load() != 1 || !strings.Contains(w.Header().Get("Location"), shaA) {
		t.Fatalf("within ttl: hits=%d location=%q", u.hits.Load(), w.Header().Get("Location"))
	}

	age(p, remote.Name, 2*time.Second)
	w, _ = serve(p, remote, path)
	if u.hits.Load() != 2 || !strings.Contains(w.Header().Get("Location"), shaB) {
		t.Fatalf("past ttl: hits=%d location=%q", u.hits.Load(), w.Header().Get("Location"))
	}
}

func TestServeRemoteBranchUpstreamFailure(t *testing.T) {
	path := "o/r/archive/refs/heads/main.tar.gz"
	for _, tc := range []struct {
		name  string
		stale bool
		want  int
	}{
		{"stale", true, http.StatusFound},
		{"strict", false, http.StatusBadGateway},
	} {
		t.Run(tc.name, func(t *testing.T) {
			u := newUpstream(t, true)
			p := New()
			remote := models.Remote{Name: tc.name, BaseURL: u.srv.URL, StaleOnError: tc.stale}
			serve(p, remote, path)
			u.status.Store(http.StatusInternalServerError)
			if w, _ := serve(p, remote, path); w.Code != tc.want {
				t.Fatalf("code = %d, want %d", w.Code, tc.want)
			}
		})
	}
}

func TestServeRemoteBranchRateLimited(t *testing.T) {
	reset := strconv.FormatInt(time.Now().Add(120*time.Second).Unix(), 10)
	for _, tc := range []struct {
		name   string
		status int
		header http.Header
		want   int
		retry  [2]int
	}{
		{"primary 403", http.StatusForbidden, http.Header{"X-Ratelimit-Remaining": {"0"}, "X-Ratelimit-Reset": {reset}}, http.StatusServiceUnavailable, [2]int{100, 122}},
		{"secondary 403", http.StatusForbidden, http.Header{"Retry-After": {"30"}}, http.StatusServiceUnavailable, [2]int{30, 31}},
		{"429", http.StatusTooManyRequests, http.Header{"Retry-After": {"5"}}, http.StatusServiceUnavailable, [2]int{5, 6}},
		{"429 no hint", http.StatusTooManyRequests, http.Header{}, http.StatusServiceUnavailable, [2]int{60, 60}},
		{"plain 403", http.StatusForbidden, http.Header{}, http.StatusBadGateway, [2]int{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			u := newUpstream(t, false)
			u.status.Store(int32(tc.status))
			u.header.Store(tc.header)
			p := New()
			p.githubAPI = u.srv.URL
			w, _ := serve(p, models.Remote{Name: "gh", BaseURL: "https://github.com"}, "o/r/archive/refs/heads/main.tar.gz")
			if w.Code != tc.want {
				t.Fatalf("code = %d, want %d", w.Code, tc.want)
			}
			ra := w.Header().Get("Retry-After")
			if tc.want != http.StatusServiceUnavailable {
				if ra != "" {
					t.Fatalf("Retry-After = %q on %d", ra, w.Code)
				}
				return
			}
			if n, err := strconv.Atoi(ra); err != nil || n < tc.retry[0] || n > tc.retry[1] {
				t.Fatalf("Retry-After = %q, want %d..%d", ra, tc.retry[0], tc.retry[1])
			}
		})
	}
}

func TestServeRemoteBranchNotFoundIsCached(t *testing.T) {
	u := newUpstream(t, false)
	u.status.Store(http.StatusNotFound)
	p := New()
	p.githubAPI = u.srv.URL
	remote := models.Remote{Name: "gh", BaseURL: "https://github.com"}
	path := "o/r/archive/refs/heads/nope.tar.gz"

	for range 3 {
		if w, _ := serve(p, remote, path); w.Code != http.StatusNotFound {
			t.Fatalf("code = %d, want 404", w.Code)
		}
	}
	if u.hits.Load() != 1 {
		t.Fatalf("hits = %d, want 1 within negative ttl", u.hits.Load())
	}
	u.status.Store(http.StatusOK)
	age(p, remote.Name, negativeTTL)
	if w, _ := serve(p, remote, path); w.Code != http.StatusFound || u.hits.Load() != 2 {
		t.Fatalf("after negative ttl: code=%d hits=%d", w.Code, u.hits.Load())
	}
}

func TestServeRemoteConcurrentLookupsShareOneCall(t *testing.T) {
	u := newUpstream(t, true)
	u.gate = make(chan struct{})
	p := New()
	remote := models.Remote{Name: "g", BaseURL: u.srv.URL, MutableTTL: 600}

	var wg sync.WaitGroup
	codes := make([]int, 20)
	for i := range codes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w, _ := serve(p, remote, "o/r/archive/refs/heads/main.tar.gz")
			codes[i] = w.Code
		}()
	}
	for u.hits.Load() == 0 {
		time.Sleep(time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond)
	close(u.gate)
	wg.Wait()
	if u.hits.Load() != 1 {
		t.Fatalf("hits = %d, want 1", u.hits.Load())
	}
	for i, c := range codes {
		if c != http.StatusFound {
			t.Fatalf("request %d: code %d", i, c)
		}
	}
}

func TestStoreBoundsEntriesPerRemote(t *testing.T) {
	p := New()
	for i := range maxRefs + 10 {
		p.store("a", fmt.Sprint(i), "")
	}
	p.store("b", "x", shaA)
	if n := len(p.refs["a"]); n != maxRefs {
		t.Fatalf("remote a holds %d entries, want %d", n, maxRefs)
	}
	if len(p.refs["b"]) != 1 {
		t.Fatalf("remote b must keep its own budget")
	}
	if _, ok := p.refs["a"][fmt.Sprint(maxRefs+9)]; !ok {
		t.Fatalf("newest entry evicted")
	}
}

func TestServeRemoteRejectsBadCommit(t *testing.T) {
	u := newUpstream(t, true)
	u.commit.Store("../../etc")
	w, _ := serve(New(), models.Remote{Name: "g", BaseURL: u.srv.URL}, "o/r/archive/refs/heads/main.tar.gz")
	if w.Code != http.StatusBadGateway {
		t.Fatalf("code = %d, want 502", w.Code)
	}
}

func TestServeRemoteAllowlist(t *testing.T) {
	u := newUpstream(t, true)
	remote := models.Remote{Name: "g", BaseURL: u.srv.URL, Patterns: []string{"^allowed/repo/"}}
	if w, _ := serve(New(), remote, "other/repo/archive/refs/heads/main.tar.gz"); w.Code != http.StatusForbidden {
		t.Fatalf("code = %d, want 403", w.Code)
	}
	if u.hits.Load() != 0 {
		t.Fatalf("denied branch must not hit the API")
	}
}

func TestServeRemoteBlockedRedirectTarget(t *testing.T) {
	u := newUpstream(t, true)
	remote := models.Remote{Name: "g", BaseURL: u.srv.URL, Blocklist: []string{"/archive/" + shaA}}
	w, _ := serve(New(), remote, "o/r/archive/refs/heads/main.tar.gz")
	if w.Code != http.StatusForbidden || w.Header().Get("Location") != "" {
		t.Fatalf("code = %d location %q, want 403 without redirect", w.Code, w.Header().Get("Location"))
	}
}

func TestServeRemotePassesThroughAndRejects(t *testing.T) {
	p := New()
	remote := models.Remote{Name: "g", BaseURL: "https://github.com"}
	if _, handled := serve(p, remote, "o/r/archive/refs/tags/v1.tar.gz"); handled {
		t.Error("tag archives must go through the proxy engine")
	}
	if _, handled := serve(p, remote, "o/r/releases/download/v1/a.bin"); handled {
		t.Error("release assets must go through the proxy engine")
	}
	if w, handled := serve(p, remote, "o/r/raw/main/x"); !handled || w.Code != http.StatusNotFound {
		t.Errorf("unsupported path: handled=%v code=%d", handled, w.Code)
	}
}
