package virtual

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/IUCLabs/artifactapi/pkg/models"
)

func fakeEngine(members map[string]*RPMMember) *Engine {
	return &Engine{
		getRemote: func(_ context.Context, name string) (*models.Remote, error) {
			return &models.Remote{Name: name, RepoType: models.RepoTypeRemote}, nil
		},
		rpmMember: func(_ context.Context, name string) (*RPMMember, error) {
			if m := members[name]; m != nil {
				return m, nil
			}
			return nil, errors.New("upstream down")
		},
	}
}

func rpmVirt(members ...string) models.Virtual {
	return models.Virtual{Name: "v", PackageType: models.PackageRPM, Members: members}
}

func TestFetchRPMFailsClosed(t *testing.T) {
	e := fakeEngine(map[string]*RPMMember{
		"a": {RemoteName: "a", Data: map[string][]byte{"primary": primaryXML(primaryPkgXML("foo", "1", "aaa", "foo.rpm"))}},
	})
	_, _, err := e.Fetch(context.Background(), rpmVirt("a", "b"), "repodata/repomd.xml", "")
	if err == nil || errors.Is(err, ErrNotFound) {
		t.Fatalf("want upstream error with a member down, got %v", err)
	}
}

func TestFetchRPMDataSurvivesMemberChange(t *testing.T) {
	m := &RPMMember{RemoteName: "a", Data: map[string][]byte{"primary": primaryXML(primaryPkgXML("foo", "1", "aaa", "foo.rpm"))}}
	e := fakeEngine(map[string]*RPMMember{"a": m})
	virt := rpmVirt("a")

	repomd, _, err := e.Fetch(context.Background(), virt, "repodata/repomd.xml", "")
	if err != nil {
		t.Fatal(err)
	}
	m.Data = map[string][]byte{"primary": primaryXML(primaryPkgXML("foo", "2", "bbb", "foo2.rpm"))}

	href := regexp.MustCompile(`repodata/[0-9a-f]+-primary\.xml\.gz`).Find(repomd)
	body, _, err := e.Fetch(context.Background(), virt, string(href), "")
	if err != nil {
		t.Fatalf("data from previous repomd must stay resolvable: %v", err)
	}
	if got := string(gunzip(t, body)); !regexp.MustCompile(`ver="1"`).MatchString(got) {
		t.Fatalf("served wrong primary:\n%s", got)
	}

	newMD, _, err := e.Fetch(context.Background(), virt, "repodata/repomd.xml", "")
	if err != nil || string(newMD) == string(repomd) {
		t.Fatalf("repomd should reflect the member change (err %v)", err)
	}
	if _, _, err := e.Fetch(context.Background(), virt, string(href), ""); err != nil {
		t.Fatalf("previous generation must survive one re-merge: %v", err)
	}
}

func TestFetchRPMLocalMemberWithoutRepodataIsUpstreamError(t *testing.T) {
	e := fakeEngine(map[string]*RPMMember{
		"a": {RemoteName: "a", Data: map[string][]byte{"primary": primaryXML(primaryPkgXML("foo", "1", "aaa", "foo.rpm"))}},
	})
	e.rpmMember = func(_ context.Context, name string) (*RPMMember, error) {
		if name == "local" {
			return nil, fmt.Errorf("local/repodata/repomd.xml: %w", ErrNotFound)
		}
		return &RPMMember{RemoteName: name, Data: map[string][]byte{"primary": primaryXML(primaryPkgXML("foo", "1", "aaa", "foo.rpm"))}}, nil
	}
	_, _, err := e.Fetch(context.Background(), rpmVirt("a", "local"), "repodata/repomd.xml", "")
	if err == nil || errors.Is(err, ErrNotFound) {
		t.Fatalf("member failure must not surface as not-found, got %v", err)
	}
}

func TestFetchRPMMergeIsShared(t *testing.T) {
	var calls atomic.Int32
	e := fakeEngine(nil)
	e.mergeTTL = time.Minute
	e.rpmMember = func(_ context.Context, name string) (*RPMMember, error) {
		calls.Add(1)
		time.Sleep(20 * time.Millisecond)
		return &RPMMember{RemoteName: name, Data: map[string][]byte{"primary": primaryXML(primaryPkgXML("foo", "1", "aaa", "foo.rpm"))}}, nil
	}
	var wg sync.WaitGroup
	for range 10 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, _, err := e.Fetch(context.Background(), rpmVirt("a"), "repodata/repomd.xml", ""); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if _, _, err := e.Fetch(context.Background(), rpmVirt("a"), "repodata/repomd.xml", ""); err != nil {
		t.Fatal(err)
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("member fetched %d times, want 1", n)
	}
}

func TestFetchRPMDataScopedToVirtual(t *testing.T) {
	e := fakeEngine(map[string]*RPMMember{
		"a": {RemoteName: "a", Data: map[string][]byte{"primary": primaryXML(primaryPkgXML("foo", "1", "aaa", "foo.rpm"))}},
		"b": {RemoteName: "b", Data: map[string][]byte{"primary": primaryXML(primaryPkgXML("bar", "1", "bbb", "bar.rpm"))}},
	})
	virtA := models.Virtual{Name: "va", PackageType: models.PackageRPM, Members: []string{"a"}}
	virtB := models.Virtual{Name: "vb", PackageType: models.PackageRPM, Members: []string{"b"}}

	repomd, _, err := e.Fetch(context.Background(), virtA, "repodata/repomd.xml", "")
	if err != nil {
		t.Fatal(err)
	}
	href := regexp.MustCompile(`repodata/[0-9a-f]+-primary\.xml\.gz`).Find(repomd)
	if _, _, err := e.Fetch(context.Background(), virtB, string(href), ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("virtual vb served va's data file (err %v)", err)
	}
}

func TestMemberRedirect(t *testing.T) {
	e := fakeEngine(nil)
	virt := rpmVirt("gh", "other")
	for _, tc := range []struct {
		path, query, want string
		ok                bool
		err               error
	}{
		{path: "gh/o/r/releases/download/v1/c++-1.rpm", query: "x=1", ok: true,
			want: "http://h/api/v1/remote/gh/o/r/releases/download/v1/c++-1.rpm?x=1"},
		{path: "gh/a%20b/c%3Fd.rpm", ok: true, want: "http://h/api/v1/remote/gh/a%20b/c%3Fd.rpm"},
		{path: "gh/../../remote/other/x", err: ErrBadPath},
		{path: "gh/%2e%2e/%2e%2e/remote/other/x", err: ErrBadPath},
		{path: "gh/a/./b", err: ErrBadPath},
		{path: "gh//etc/passwd", err: ErrBadPath},
		{path: "repodata/repomd.xml"},
		{path: "nope/x.rpm"},
	} {
		got, ok, err := e.MemberRedirect(context.Background(), virt, tc.path, tc.query, "http://h/")
		if got != tc.want || ok != tc.ok || !errors.Is(err, tc.err) {
			t.Errorf("%s: got (%q, %v, %v), want (%q, %v, %v)", tc.path, got, ok, err, tc.want, tc.ok, tc.err)
		}
	}
}

func debEngine(members map[string]*DebMember) *Engine {
	e := fakeEngine(nil)
	e.debMember = func(_ context.Context, name, base string) (*DebMember, error) {
		if m := members[name]; m != nil && base == "" {
			return m, nil
		}
		return nil, errors.New("upstream down")
	}
	return e
}

func debVirt(members ...string) models.Virtual {
	return models.Virtual{Name: "v", PackageType: models.PackageDeb, Members: members}
}

func TestFetchDebFailsClosed(t *testing.T) {
	a := flatDebMember("a", "", debStanza("foo", "1", "amd64", "pool/foo.deb"))
	e := debEngine(map[string]*DebMember{"a": &a})
	for _, p := range []string{"Release", "./Packages.gz", "dists/trixie/Release"} {
		if _, _, err := e.Fetch(context.Background(), debVirt("a", "b"), p, ""); err == nil || errors.Is(err, ErrNotFound) {
			t.Errorf("%s: want upstream error with a member down, got %v", p, err)
		}
	}
	for _, p := range []string{"InRelease", "./Release.gpg", "Packages.xz"} {
		if _, _, err := e.Fetch(context.Background(), debVirt("a"), p, ""); !errors.Is(err, ErrNotFound) {
			t.Errorf("%s: want not found, got %v", p, err)
		}
	}
}

func TestFetchDebRemergesAfterTTL(t *testing.T) {
	a := flatDebMember("a", "", debStanza("foo", "1", "amd64", "pool/foo_1.deb"))
	e := debEngine(map[string]*DebMember{"a": &a})
	virt := debVirt("a")

	rel, _, err := e.Fetch(context.Background(), virt, "./Release", "")
	if err != nil {
		t.Fatal(err)
	}
	a.Packages[""] = []byte(debStanza("foo", "2", "amd64", "pool/foo_2.deb"))
	e.mergeTTL = 0
	newRel, _, err := e.Fetch(context.Background(), virt, "Release", "")
	if err != nil || string(newRel) == string(rel) {
		t.Fatalf("Release should reflect the member change (err %v)", err)
	}
	pk, ct, err := e.Fetch(context.Background(), virt, "./Packages", "")
	if err != nil || ct != "text/plain" || !strings.Contains(string(pk), "Version: 2") {
		t.Fatalf("Packages should be the current merge (ct %q, err %v):\n%s", ct, err, pk)
	}
}

func TestFetchDebMergeIsSharedPerSuite(t *testing.T) {
	var calls atomic.Int32
	e := fakeEngine(nil)
	e.mergeTTL = time.Minute
	e.debMember = func(_ context.Context, name, base string) (*DebMember, error) {
		calls.Add(1)
		m := flatDebMember(name, "", debStanza("foo", "1", "amd64", "pool/foo.deb"))
		return &m, nil
	}
	for _, p := range []string{"Release", "Packages", "Packages.gz", "./Release", "dists/a/Release", "dists/b/Release"} {
		if _, _, err := e.Fetch(context.Background(), debVirt("a"), p, ""); err != nil && !errors.Is(err, ErrNotFound) {
			t.Fatal(err)
		}
	}
	if n := calls.Load(); n != 3 {
		t.Fatalf("member fetched %d times, want once per Release dir (3)", n)
	}
}

func TestMemberRedirectDeb(t *testing.T) {
	e := fakeEngine(nil)
	virt := debVirt("gh", "loc")
	for _, tc := range []struct {
		path, want string
		ok         bool
		err        error
	}{
		{path: "gh/o/r/releases/download/v1/foo_1.0_amd64.deb", ok: true, want: "http://h/api/v1/remote/gh/o/r/releases/download/v1/foo_1.0_amd64.deb"},
		{path: "./gh/pool/a+b.deb", ok: true, want: "http://h/api/v1/remote/gh/pool/a+b.deb"},
		{path: "gh/../loc/x.deb", err: ErrBadPath},
		{path: "dists/trixie/Release"},
		{path: "Packages"},
	} {
		got, ok, err := e.MemberRedirect(context.Background(), virt, tc.path, "", "http://h/")
		if got != tc.want || ok != tc.ok || !errors.Is(err, tc.err) {
			t.Errorf("%s: got (%q, %v, %v), want (%q, %v, %v)", tc.path, got, ok, err, tc.want, tc.ok, tc.err)
		}
	}
}

type fakeRepo struct {
	idx string
	fs  map[string][]byte
}

func (r *fakeRepo) index() []byte            { return []byte(r.idx) }
func (r *fakeRepo) files() map[string][]byte { return r.fs }

func genRepo(gen string) func(context.Context) (mergedRepo, error) {
	return func(context.Context) (mergedRepo, error) {
		return &fakeRepo{idx: gen, fs: map[string][]byte{gen + ".gz": []byte(gen), "fixed": []byte(gen)}}, nil
	}
}

func TestMergedRetainsPreviousGeneration(t *testing.T) {
	e := fakeEngine(nil)
	for _, gen := range []string{"g1", "g2", "g3"} {
		if _, err := e.merged(context.Background(), "k", genRepo(gen)); err != nil {
			t.Fatal(err)
		}
	}
	for path, want := range map[string]string{"g3.gz": "g3", "g2.gz": "g2", "fixed": "g3"} {
		if body, ok := e.cachedFile("k", path); !ok || string(body) != want {
			t.Errorf("%s: got (%q, %v), want %q", path, body, ok, want)
		}
	}
	if _, ok := e.cachedFile("k", "g1.gz"); ok {
		t.Error("generation two merges back must be dropped")
	}
}

func TestMergedReusesWithinTTLAndIsolatesKeys(t *testing.T) {
	e := fakeEngine(nil)
	e.mergeTTL = time.Minute
	for _, step := range []struct{ key, gen, want string }{
		{"a", "a1", "a1"}, {"b", "b1", "b1"}, {"a", "a2", "a1"},
	} {
		repo, err := e.merged(context.Background(), step.key, genRepo(step.gen))
		if err != nil || string(repo.index()) != step.want {
			t.Fatalf("%s: got %v (err %v), want %s", step.key, repo, err, step.want)
		}
	}
	if _, ok := e.cachedFile("b", "a1.gz"); ok {
		t.Error("key b served key a's file")
	}
	if _, ok := e.cachedFile("c", "a1.gz"); ok {
		t.Error("unknown key served a file")
	}
}

func suiteDebMember(name, version string) *DebMember {
	return &DebMember{RemoteName: name, Release: []byte("Suite: s\nSHA256:\n x 1 main/binary-amd64/Packages.gz\n"),
		Packages: map[string][]byte{"main/binary-amd64/": []byte(debStanza("foo", version, "amd64", "pool/foo_"+version+".deb"))}}
}

func TestFetchDebByHashSurvivesRemerge(t *testing.T) {
	version := "1"
	e := fakeEngine(nil)
	e.debMember = func(_ context.Context, name, _ string) (*DebMember, error) { return suiteDebMember(name, version), nil }
	virt := debVirt("a")
	fetch := func(p string) []byte {
		body, _, err := e.Fetch(context.Background(), virt, p, "")
		if err != nil {
			t.Fatalf("%s: %v", p, err)
		}
		return body
	}
	byHash := func(rel []byte) string {
		m := regexp.MustCompile(`(?m)^SHA256:\n(?: .*\n)*? ([0-9a-f]{64}) \d+ main/binary-amd64/Packages.gz$`).FindSubmatch(rel)
		if m == nil {
			t.Fatalf("no Packages.gz SHA256 in Release:\n%s", rel)
		}
		return "dists/trixie/main/binary-amd64/by-hash/SHA256/" + string(m[1])
	}

	oldRel := fetch("dists/trixie/Release")
	version, e.mergeTTL = "2", 0
	if newRel := fetch("dists/trixie/Release"); string(newRel) == string(oldRel) {
		t.Fatal("Release should reflect the member change")
	}
	if got := string(gunzip(t, fetch(byHash(oldRel)))); !strings.Contains(got, "Version: 1\n") {
		t.Fatalf("by-hash from the previous Release served the wrong index:\n%s", got)
	}
	if got := string(fetch("dists/trixie/main/binary-amd64/Packages")); !strings.Contains(got, "Version: 2\n") {
		t.Fatalf("fixed-name Packages should be the current merge:\n%s", got)
	}
}

func TestFetchDebBoundsSuitesPerVirtual(t *testing.T) {
	e := fakeEngine(nil)
	e.mergeTTL = time.Minute
	e.debMember = func(_ context.Context, name, _ string) (*DebMember, error) { return suiteDebMember(name, "1"), nil }
	other := models.Virtual{Name: "other", PackageType: models.PackageDeb, Members: []string{"a"}}
	if _, _, err := e.Fetch(context.Background(), other, "dists/keep/Release", ""); err != nil {
		t.Fatal(err)
	}
	for i := range maxDebSuites + 5 {
		if _, _, err := e.Fetch(context.Background(), debVirt("a"), fmt.Sprintf("dists/s%d/Release", i), ""); err != nil {
			t.Fatal(err)
		}
		time.Sleep(time.Microsecond)
	}
	n := 0
	for k := range e.gens {
		if strings.HasPrefix(k, "deb\x00v\x00") {
			n++
		}
	}
	if n != maxDebSuites {
		t.Fatalf("virtual v caches %d suites, want %d", n, maxDebSuites)
	}
	if e.gens["deb\x00v\x00dists/s0/"] != nil || e.gens["deb\x00v\x00dists/s4/"] != nil {
		t.Fatal("least recently used suites should be evicted first")
	}
	if e.gens["deb\x00other\x00dists/keep/"] == nil {
		t.Fatal("another virtual's suites must not be evicted")
	}
}
