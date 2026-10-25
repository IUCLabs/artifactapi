package virtual

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/IUCLabs/artifactapi/pkg/models"
)

func rpmPkgMember(name, ver string) *RPMMember {
	return &RPMMember{RemoteName: name, Data: map[string][]byte{"primary": primaryXML(primaryPkgXML("foo", ver, "h"+ver, "foo-"+ver+".rpm"))}}
}

// staleEngine re-merges on every request; member "a" is fetched through fetch
// and has serve_stale set to stale.
func staleEngine(stale bool, fetch func() *RPMMember) (*Engine, *atomic.Int32) {
	e, calls, _ := staleEngineErr(stale, func() (*RPMMember, error) { return fetch(), nil })
	return e, calls
}

func staleEngineErr(stale bool, fetch func() (*RPMMember, error)) (*Engine, *atomic.Int32, *atomic.Bool) {
	var calls atomic.Int32
	var flag atomic.Bool
	flag.Store(stale)
	e := fakeEngine(nil)
	e.getRemote = func(_ context.Context, name string) (*models.Remote, error) {
		return &models.Remote{Name: name, RepoType: models.RepoTypeRemote, ServeStale: flag.Load()}, nil
	}
	e.rpmMember = func(_ context.Context, name string, _ bool) (*RPMMember, error) {
		calls.Add(1)
		return fetch()
	}
	return e, &calls, &flag
}

// waitRefresh blocks until the background refresh of sk, if any, has finished.
func waitRefresh(e *Engine, sk string) {
	_, _, _ = e.sf.Do("stale\x00"+sk, func() (any, error) { return nil, nil })
}

const rpmStaleKey = "rpm\x00v\x00a"

func fetchRepomd(t *testing.T, e *Engine) string {
	t.Helper()
	b, _, err := e.Fetch(context.Background(), rpmVirt("a"), "repodata/repomd.xml", "")
	if err != nil {
		t.Error(err)
	}
	return string(b)
}

func TestServeStaleMemberDoesNotBlockMerge(t *testing.T) {
	release := make(chan struct{})
	refreshing := make(chan struct{}, 1)
	var slow atomic.Bool
	e, calls := staleEngine(true, func() *RPMMember {
		if slow.Load() {
			select {
			case refreshing <- struct{}{}:
			default:
			}
			<-release
			return rpmPkgMember("a", "2")
		}
		return rpmPkgMember("a", "1")
	})
	v1 := fetchRepomd(t, e)
	slow.Store(true)

	for range 3 {
		done := make(chan string)
		go func() { done <- fetchRepomd(t, e) }()
		select {
		case got := <-done:
			if got != v1 {
				t.Fatal("stale merge should serve the previous member copy")
			}
		case <-time.After(time.Second):
			t.Fatal("merge blocked on a serve_stale member refresh")
		}
	}
	select {
	case <-refreshing:
	case <-time.After(5 * time.Second):
		t.Fatal("background refresh never started")
	}
	if n := calls.Load(); n != 2 {
		t.Fatalf("member fetched %d times, want 2 (cold + one single-flighted refresh)", n)
	}

	close(release)
	deadline := time.Now().Add(5 * time.Second)
	for fetchRepomd(t, e) == v1 {
		if time.Now().After(deadline) {
			t.Fatal("background refresh never reached a merge")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func assertMergeBlocks(t *testing.T, e *Engine, release chan struct{}) {
	t.Helper()
	done := make(chan struct{})
	go func() { fetchRepomd(t, e); close(done) }()
	select {
	case <-done:
		t.Fatal("merge returned before the member fetch finished")
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	<-done
}

func TestNonServeStaleMemberBlocksMerge(t *testing.T) {
	release := make(chan struct{})
	var slow atomic.Bool
	e, _ := staleEngine(false, func() *RPMMember {
		if slow.Load() {
			<-release
		}
		return rpmPkgMember("a", "1")
	})
	fetchRepomd(t, e)
	slow.Store(true)
	assertMergeBlocks(t, e, release)
}

func TestServeStaleColdStartBlocksMerge(t *testing.T) {
	release := make(chan struct{})
	e, _ := staleEngine(true, func() *RPMMember {
		<-release
		return rpmPkgMember("a", "1")
	})
	assertMergeBlocks(t, e, release)
}

func TestServeStaleRefreshErrorKeepsCopy(t *testing.T) {
	var down atomic.Bool
	e, _, _ := staleEngineErr(true, func() (*RPMMember, error) {
		if down.Load() {
			return nil, errors.New("upstream down")
		}
		return rpmPkgMember("a", "1"), nil
	})
	v1 := fetchRepomd(t, e)
	down.Store(true)
	for range 3 {
		if got := fetchRepomd(t, e); got != v1 {
			t.Fatal("a failed refresh should keep serving the previous copy")
		}
		waitRefresh(e, rpmStaleKey)
	}
}

func TestServeStaleMaxAgeFallsBackToFetch(t *testing.T) {
	var down atomic.Bool
	e, _, _ := staleEngineErr(true, func() (*RPMMember, error) {
		if down.Load() {
			return nil, errors.New("upstream down")
		}
		return rpmPkgMember("a", "1"), nil
	})
	fetchRepomd(t, e)
	down.Store(true)
	e.mu.Lock()
	e.stale[rpmStaleKey].at = time.Now().Add(-maxStaleAge)
	e.mu.Unlock()
	if _, _, err := e.Fetch(context.Background(), rpmVirt("a"), "repodata/repomd.xml", ""); err == nil {
		t.Fatal("a copy past maxStaleAge should not be served")
	}
	if len(e.stale) != 0 {
		t.Fatal("an expired copy should be dropped")
	}
}

func TestServeStaleToggleOffClearsCopy(t *testing.T) {
	var down atomic.Bool
	e, _, flag := staleEngineErr(true, func() (*RPMMember, error) {
		if down.Load() {
			return nil, errors.New("upstream down")
		}
		return rpmPkgMember("a", "1"), nil
	})
	fetchRepomd(t, e)
	flag.Store(false)
	fetchRepomd(t, e)
	if len(e.stale) != 0 {
		t.Fatal("turning serve_stale off should drop the copy")
	}
	flag.Store(true)
	down.Store(true)
	if _, _, err := e.Fetch(context.Background(), rpmVirt("a"), "repodata/repomd.xml", ""); err == nil {
		t.Fatal("re-enabling serve_stale must not resurrect the old copy")
	}
}

func staleRemote(_ context.Context, name string) (*models.Remote, error) {
	return &models.Remote{Name: name, RepoType: models.RepoTypeRemote, ServeStale: true}, nil
}

func TestServeStaleDebAndAlpine(t *testing.T) {
	var down atomic.Bool
	e := fakeEngine(nil)
	e.getRemote = staleRemote
	e.debMember = func(_ context.Context, name, _ string, _ bool) (*DebMember, error) {
		if down.Load() {
			return nil, errors.New("upstream down")
		}
		return suiteDebMember(name, "1"), nil
	}
	e.apkMember = func(_ context.Context, name, _ string) (*AlpineMember, error) {
		if down.Load() {
			return nil, errors.New("upstream down")
		}
		return &AlpineMember{RemoteName: name, Index: []byte(apkRec("foo", "1-r0", "x86_64"))}, nil
	}
	for _, tc := range []struct {
		virt models.Virtual
		path string
	}{
		{debVirt("a"), "dists/s/Release"},
		{alpineVirt("a"), "x86_64/APKINDEX.tar.gz"},
	} {
		down.Store(false)
		first, _, err := e.Fetch(context.Background(), tc.virt, tc.path, "")
		if err != nil {
			t.Fatal(err)
		}
		down.Store(true)
		got, _, err := e.Fetch(context.Background(), tc.virt, tc.path, "")
		if err != nil || string(got) != string(first) {
			t.Fatalf("%s: want the previous copy while the member is down, got err %v", tc.virt.PackageType, err)
		}
	}
}

func TestServeStaleEvictionDropsLateRefresh(t *testing.T) {
	release := make(chan struct{})
	var slow atomic.Bool
	e := fakeEngine(nil)
	e.mergeTTL = time.Minute
	e.getRemote = staleRemote
	e.debMember = func(_ context.Context, name, base string, _ bool) (*DebMember, error) {
		if base == "dists/s0/" && slow.Load() {
			<-release
		}
		return suiteDebMember(name, "1"), nil
	}
	fetch := func(suite string) {
		t.Helper()
		if _, _, err := e.Fetch(context.Background(), debVirt("a"), "dists/"+suite+"/Release", ""); err != nil {
			t.Fatal(err)
		}
	}
	fetch("s0")
	slow.Store(true)
	e.mu.Lock()
	e.gens["deb\x00v\x00dists/s0/"].at = time.Time{}
	e.mu.Unlock()
	fetch("s0")
	for i := range maxDebSuites {
		time.Sleep(time.Microsecond)
		fetch(fmt.Sprintf("s%d", i+1))
	}
	close(release)
	sk := "deb\x00v\x00dists/s0/\x00a"
	waitRefresh(e, sk)
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.gens["deb\x00v\x00dists/s0/"] != nil {
		t.Fatal("suite s0 should be evicted")
	}
	if e.stale[sk] != nil {
		t.Fatal("a refresh finishing after eviction must not re-insert its copy")
	}
	if len(e.stale) != maxDebSuites {
		t.Fatalf("stale holds %d copies, want %d", len(e.stale), maxDebSuites)
	}
}

func TestServeStaleEvictionDropsLateColdFetch(t *testing.T) {
	release := make(chan struct{})
	var slow atomic.Bool
	e := fakeEngine(nil)
	e.mergeTTL = time.Minute
	e.getRemote = staleRemote
	e.debMember = func(_ context.Context, name, base string, _ bool) (*DebMember, error) {
		if base == "dists/s0/" && slow.Load() {
			<-release
		}
		return suiteDebMember(name, "1"), nil
	}
	fetch := func(suite string) {
		if _, _, err := e.Fetch(context.Background(), debVirt("a"), "dists/"+suite+"/Release", ""); err != nil {
			t.Error(err)
		}
	}
	sk := "deb\x00v\x00dists/s0/\x00a"
	fetch("s0")
	slow.Store(true)
	e.mu.Lock()
	e.gens["deb\x00v\x00dists/s0/"].at = time.Time{}
	delete(e.stale, sk)
	e.mu.Unlock()
	done := make(chan struct{})
	go func() { defer close(done); fetch("s0") }()
	for {
		e.mu.Lock()
		cold := e.stale[sk] != nil
		e.mu.Unlock()
		if cold {
			break
		}
		time.Sleep(time.Millisecond)
	}
	for i := range maxDebSuites {
		time.Sleep(time.Microsecond)
		fetch(fmt.Sprintf("s%d", i+1))
	}
	close(release)
	<-done
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.stale[sk] != nil {
		t.Fatal("a cold fetch finishing after eviction must not re-insert its copy")
	}
}

func TestMergePrunesExpiredCopiesOfOtherMembers(t *testing.T) {
	e, _ := staleEngine(false, func() *RPMMember { return rpmPkgMember("a", "1") })
	e.stale = map[string]*staleEntry{"rpm\x00gone\x00x": {m: rpmPkgMember("x", "1"), at: time.Now().Add(-maxStaleAge)}}
	fetchRepomd(t, e)
	if len(e.stale) != 0 {
		t.Fatal("any merge should prune expired copies of removed members")
	}
}
