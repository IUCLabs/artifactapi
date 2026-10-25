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
		rpmMember: func(_ context.Context, name string, _ bool) (*RPMMember, error) {
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
	e.rpmMember = func(_ context.Context, name string, _ bool) (*RPMMember, error) {
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
	e.rpmMember = func(_ context.Context, name string, _ bool) (*RPMMember, error) {
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
	e.debMember = func(_ context.Context, name, base string, _ bool) (*DebMember, error) {
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
	e.debMember = func(_ context.Context, name, base string, _ bool) (*DebMember, error) {
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
	e.debMember = func(_ context.Context, name, _ string, _ bool) (*DebMember, error) {
		return suiteDebMember(name, version), nil
	}
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
	e.debMember = func(_ context.Context, name, _ string, _ bool) (*DebMember, error) {
		return suiteDebMember(name, "1"), nil
	}
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

func fetchFilesInput(n int) map[string]string {
	files := map[string]string{}
	for i := range n {
		files[fmt.Sprintf("k%d", i)] = fmt.Sprintf("f%d", i)
	}
	return files
}

func TestFetchFilesParallelRunsConcurrently(t *testing.T) {
	var inFlight, peak atomic.Int32
	release := make(chan struct{})
	go func() {
		for peak.Load() < maxFileFetches {
			time.Sleep(time.Millisecond)
		}
		close(release)
	}()
	got, err := fetchFiles(context.Background(), true, fetchFilesInput(6), func(_ context.Context, f string) ([]byte, error) {
		n := inFlight.Add(1)
		defer inFlight.Add(-1)
		for p := peak.Load(); n > p && !peak.CompareAndSwap(p, n); p = peak.Load() {
		}
		select {
		case <-release:
		case <-time.After(5 * time.Second):
			return nil, errors.New("fetches never overlapped")
		}
		return []byte(f), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if peak.Load() != maxFileFetches {
		t.Fatalf("peak concurrency %d, want %d", peak.Load(), maxFileFetches)
	}
	for k, f := range fetchFilesInput(6) {
		if string(got[k]) != f {
			t.Fatalf("%s = %q, want %q", k, got[k], f)
		}
	}
}

func TestFetchFilesSerialByDefault(t *testing.T) {
	var inFlight atomic.Int32
	_, err := fetchFiles(context.Background(), false, fetchFilesInput(4), func(_ context.Context, f string) ([]byte, error) {
		defer inFlight.Add(-1)
		if inFlight.Add(1) > 1 {
			return nil, errors.New("concurrent fetch")
		}
		time.Sleep(time.Millisecond)
		return []byte(f), nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestFetchFilesErrorFailsMember(t *testing.T) {
	boom := errors.New("boom")
	for _, parallel := range []bool{false, true} {
		got, err := fetchFiles(context.Background(), parallel, fetchFilesInput(5), func(_ context.Context, f string) ([]byte, error) {
			if f == "f2" {
				return nil, boom
			}
			return []byte(f), nil
		})
		if !errors.Is(err, boom) || got != nil {
			t.Fatalf("parallel=%v: got %v, %v; want boom and no files", parallel, got, err)
		}
	}
}

func TestMergeRPMPassesParallelFetchAndKeepsMemberOrder(t *testing.T) {
	e := fakeEngine(nil)
	var sawParallel atomic.Bool
	e.rpmMember = func(_ context.Context, name string, parallel bool) (*RPMMember, error) {
		sawParallel.Store(parallel)
		if name == "a" {
			time.Sleep(20 * time.Millisecond)
		}
		return &RPMMember{RemoteName: name, Data: map[string][]byte{"primary": primaryXML(primaryPkgXML("foo", "1", name+"sum", name+".rpm"))}}, nil
	}
	virt := rpmVirt("a", "b")
	virt.ParallelFetch = true
	repo, err := e.mergeRPM(context.Background(), virt)
	if err != nil {
		t.Fatal(err)
	}
	if !sawParallel.Load() {
		t.Fatal("parallel_fetch not passed to member fetch")
	}
	primary := regexp.MustCompile(`repodata/[0-9a-f]+-primary\.xml\.gz`).FindString(string(repo.Repomd))
	body := string(gunzip(t, repo.Files[primary]))
	if a, b := strings.Index(body, "a.rpm"), strings.Index(body, "b.rpm"); a < 0 || b >= 0 && b < a {
		t.Fatalf("member priority order lost:\n%s", body)
	}
}

func TestFetchFilesErrorCancelsRest(t *testing.T) {
	boom := errors.New("boom")
	var calls atomic.Int32
	_, err := fetchFiles(context.Background(), true, fetchFilesInput(8), func(ctx context.Context, _ string) ([]byte, error) {
		if calls.Add(1) == 1 {
			return nil, boom
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(5 * time.Second):
			return nil, errors.New("in-flight fetch not cancelled")
		}
	})
	if !errors.Is(err, boom) {
		t.Fatalf("got %v, want boom", err)
	}
	if n := calls.Load(); n > maxFileFetches {
		t.Fatalf("%d fetches started after the first error, want at most %d", n, maxFileFetches)
	}
}

// overlapEngine serves fixed member index paths and records the peak number of
// index file fetches in flight. Each fetch blocks until want are in flight (or
// fails after 5s); with want 1 it also holds briefly so a stray overlap shows.
func overlapEngine(index map[string][]byte, want int32, peak *atomic.Int32) *Engine {
	e := fakeEngine(nil)
	var inFlight atomic.Int32
	e.memberPath = func(_ context.Context, _ models.Remote, path string) ([]byte, error) {
		body, ok := index[path]
		if !ok {
			return nil, fmt.Errorf("%s: %w", path, ErrNotFound)
		}
		if strings.HasSuffix(path, "Release") || strings.HasSuffix(path, "repomd.xml") {
			return body, nil
		}
		n := inFlight.Add(1)
		defer inFlight.Add(-1)
		for p := peak.Load(); n > p && !peak.CompareAndSwap(p, n); p = peak.Load() {
		}
		for deadline := time.Now().Add(5 * time.Second); peak.Load() < want; {
			if time.Now().After(deadline) {
				return nil, fmt.Errorf("%s: only %d of %d fetches in flight", path, peak.Load(), want)
			}
			time.Sleep(time.Millisecond)
		}
		if want == 1 {
			time.Sleep(10 * time.Millisecond)
		}
		return body, nil
	}
	return e
}

func TestFetchRPMMemberHonoursParallelFetch(t *testing.T) {
	index := map[string][]byte{
		"repodata/repomd.xml": []byte(`<repomd>` +
			`<data type="primary"><location href="repodata/p.xml"/></data>` +
			`<data type="filelists"><location href="repodata/f.xml"/></data>` +
			`<data type="other"><location href="repodata/o.xml"/></data></repomd>`),
		"repodata/p.xml": []byte("P"), "repodata/f.xml": []byte("F"), "repodata/o.xml": []byte("O"),
	}
	for parallel, want := range map[bool]int32{false: 1, true: 3} {
		var peak atomic.Int32
		m, err := overlapEngine(index, want, &peak).fetchRPMMember(context.Background(), "a", parallel)
		if err != nil {
			t.Fatal(err)
		}
		if peak.Load() != want {
			t.Errorf("parallel=%v: peak concurrency %d, want %d", parallel, peak.Load(), want)
		}
		if string(m.Data["primary"]) != "P" || string(m.Data["filelists"]) != "F" || string(m.Data["other"]) != "O" {
			t.Errorf("parallel=%v: data %q", parallel, m.Data)
		}
	}
}

func TestFetchDebMemberHonoursParallelFetch(t *testing.T) {
	index := map[string][]byte{
		"dists/s/Release": []byte("Suite: s\nSHA256:\n" +
			" aa 1 main/binary-amd64/Packages\n" +
			" bb 1 contrib/binary-amd64/Packages\n" +
			" cc 1 non-free/binary-amd64/Packages\n"),
		"dists/s/main/binary-amd64/Packages":     []byte("M"),
		"dists/s/contrib/binary-amd64/Packages":  []byte("C"),
		"dists/s/non-free/binary-amd64/Packages": []byte("N"),
	}
	for parallel, want := range map[bool]int32{false: 1, true: 3} {
		var peak atomic.Int32
		m, err := overlapEngine(index, want, &peak).fetchDebMember(context.Background(), "a", "dists/s/", parallel)
		if err != nil {
			t.Fatal(err)
		}
		if peak.Load() != want {
			t.Errorf("parallel=%v: peak concurrency %d, want %d", parallel, peak.Load(), want)
		}
		if string(m.Packages["main/binary-amd64/"]) != "M" || string(m.Packages["non-free/binary-amd64/"]) != "N" {
			t.Errorf("parallel=%v: packages %q", parallel, m.Packages)
		}
	}
}
