package virtual

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/IUCLabs/artifactapi/internal/provider/gems"
	"github.com/IUCLabs/artifactapi/pkg/models"
)

func TestMergeGemsInfoMemberOrderWins(t *testing.T) {
	local := GemsMember{RemoteName: "internal", Body: []byte("---\n1.0.0 |checksum:aa\n1.0.0-java |checksum:ab\n")}
	remote := GemsMember{RemoteName: "rubygems", Body: []byte("---\n0.9.0 |checksum:99\n1.0.0 |checksum:bb\n2.0.0 rack:>= 0|checksum:cc,ruby:>= 3.0\n")}
	d, err := MergeGemsInfo([]GemsMember{local, {RemoteName: "empty"}, remote})
	if err != nil {
		t.Fatal(err)
	}
	want := "---\n1.0.0 |checksum:aa\n1.0.0-java |checksum:ab\n0.9.0 |checksum:99\n2.0.0 rack:>= 0|checksum:cc,ruby:>= 3.0\n"
	if string(d.Body) != want {
		t.Fatalf("merged info =\n%q\nwant\n%q", d.Body, want)
	}
	for k, owner := range map[string]string{"1.0.0": "internal", "1.0.0-java": "internal", "0.9.0": "rubygems", "2.0.0": "rubygems"} {
		if d.Owners[k] != owner {
			t.Errorf("owner of %s = %q, want %q", k, d.Owners[k], owner)
		}
	}

	odd := GemsMember{RemoteName: "rubygems", Body: []byte("---\n1.0.0 |checksum:bb\n\n")}
	if d, _ := MergeGemsInfo([]GemsMember{{RemoteName: "internal"}, odd}); string(d.Body) != string(odd.Body) {
		t.Fatalf("a gem only one member has must pass through verbatim, got %q", d.Body)
	}
	if _, err := MergeGemsInfo([]GemsMember{{RemoteName: "a"}}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func TestMergeGemsVersions(t *testing.T) {
	local := GemsMember{RemoteName: "internal", Body: []byte("created_at: 2026-10-18T00:00:00Z\n---\nhello 0.1.0 11\nrack 9.0.0 22\n")}
	remote := GemsMember{RemoteName: "rubygems", Body: []byte("created_at: 2024-04-01T00:00:05Z\n---\n" +
		"rack 1.0.0,2.0.0 33\nrails 7.0.0 44\nrack -1.0.0,3.0.0 55\n")}
	merged := &GemsDoc{Body: []byte("---\n9.0.0 |checksum:x\n2.0.0 |checksum:y\n3.0.0 |checksum:z\n")}
	var asked []string
	at := time.Date(2026, 10, 18, 1, 2, 3, 0, time.UTC)
	body, err := MergeGemsVersions([]GemsMember{local, remote}, func(name string) (*GemsDoc, error) {
		asked = append(asked, name)
		return merged, nil
	}, at)
	if err != nil {
		t.Fatal(err)
	}
	want := "created_at: 2026-10-18T01:02:03Z\n---\nhello 0.1.0 11\nrails 7.0.0 44\nrack 9.0.0,2.0.0,3.0.0 " + gems.MD5Hex(merged.Body) + "\n"
	if string(body) != want {
		t.Fatalf("versions =\n%q\nwant\n%q", body, want)
	}
	if len(asked) != 1 || asked[0] != "rack" {
		t.Fatalf("merged info fetched for %v, want only the shared gem", asked)
	}

	if _, err := MergeGemsVersions([]GemsMember{{RemoteName: "x", Body: []byte("no separator")}}, nil, at); err == nil {
		t.Fatal("malformed member versions must fail")
	}
	if _, err := MergeGemsVersions([]GemsMember{{RemoteName: "x"}}, nil, at); err == nil {
		t.Fatal("a member without versions must fail")
	}
}

func TestMergeGemsNames(t *testing.T) {
	body, err := MergeGemsNames([]GemsMember{{RemoteName: "a", Body: []byte("---\nzeta\nalpha\n")}, {RemoteName: "b", Body: []byte("---\nalpha\nmid\n")}})
	if err != nil || string(body) != "---\nalpha\nmid\nzeta\n" {
		t.Fatalf("names = %q %v", body, err)
	}
}

func gemsEngine(fetch func(name, path string) (*GemsMember, error)) *Engine {
	return &Engine{
		mergeTTL: time.Minute,
		getRemote: func(_ context.Context, name string) (*models.Remote, error) {
			rt := models.RepoTypeRemote
			if name == "internal" {
				rt = models.RepoTypeLocal
			}
			return &models.Remote{Name: name, RepoType: rt}, nil
		},
		gemsMember: func(_ context.Context, name, path string) (*GemsMember, error) { return fetch(name, path) },
	}
}

func gemsVirt(members ...string) models.Virtual {
	return models.Virtual{Name: "gems", PackageType: models.PackageGems, Members: members}
}

func TestGemsRedirectToOwner(t *testing.T) {
	var mu sync.Mutex
	asked := map[string]int{}
	e := gemsEngine(func(name, path string) (*GemsMember, error) {
		mu.Lock()
		asked[name+" "+path]++
		mu.Unlock()
		switch name + " " + path {
		case "internal info/foo-2fa":
			return &GemsMember{RemoteName: name, Body: []byte("---\n1.0.0 |checksum:a\n")}, nil
		case "rubygems info/foo-2fa":
			return &GemsMember{RemoteName: name, Body: []byte("---\n1.0.0 |checksum:b\n1.1.0-x86_64-linux |checksum:c\n")}, nil
		case "rubygems info/foo":
			return &GemsMember{RemoteName: name, Body: []byte("---\n2.0.0 |checksum:d\n")}, nil
		}
		return &GemsMember{RemoteName: name}, nil
	})
	virt := gemsVirt("internal", "rubygems")
	for path, want := range map[string]string{
		"gems/foo-2fa-1.0.0.gem":                     "http://host/api/v1/local/internal/gems/foo-2fa-1.0.0.gem",
		"gems/foo-2fa-1.1.0-x86_64-linux.gem":        "http://host/api/v1/remote/rubygems/gems/foo-2fa-1.1.0-x86_64-linux.gem",
		"quick/Marshal.4.8/foo-2fa-1.0.0.gemspec.rz": "http://host/api/v1/local/internal/quick/Marshal.4.8/foo-2fa-1.0.0.gemspec.rz",
		"quick/Marshal.4.8/foo-2.0.0.gemspec.rz":     "http://host/api/v1/remote/rubygems/quick/Marshal.4.8/foo-2.0.0.gemspec.rz",
	} {
		loc, err := e.GemsRedirect(context.Background(), virt, path, "", "http://host")
		if err != nil || loc != want {
			t.Errorf("%s -> %q %v, want %q", path, loc, err, want)
		}
	}
	for _, path := range []string{"gems/foo-2fa-9.9.9.gem", "gems/nodash.gem", "gems/x/../foo-2.0.0.gem", "other/foo-2.0.0.gem", "specs.4.8.gz"} {
		if _, err := e.GemsRedirect(context.Background(), virt, path, "", "http://host"); !errors.Is(err, ErrNotFound) {
			t.Errorf("%s: want ErrNotFound, got %v", path, err)
		}
	}
	if asked["internal info/foo-2fa"] != 1 {
		t.Fatalf("merged info must be cached across redirects, internal asked %d times", asked["internal info/foo-2fa"])
	}
}

func TestFetchGemsFailsClosed(t *testing.T) {
	e := gemsEngine(func(name, path string) (*GemsMember, error) {
		if name == "rubygems" {
			return nil, errors.New("upstream down")
		}
		return &GemsMember{RemoteName: name, Body: []byte("created_at: x\n---\nhello 0.1.0 11\n")}, nil
	})
	for _, path := range []string{"versions", "names", "info/hello"} {
		if _, _, err := e.FetchGems(context.Background(), gemsVirt("internal", "rubygems"), path); err == nil || errors.Is(err, ErrNotFound) {
			t.Errorf("%s: want member error, got %v", path, err)
		}
	}
	if _, err := e.GemsRedirect(context.Background(), gemsVirt("internal", "rubygems"), "gems/hello-0.1.0.gem", "", "http://h"); err == nil || errors.Is(err, ErrNotFound) {
		t.Errorf("redirect: want member error, got %v", err)
	}
}

func TestFetchGemsVersionsUsesMergedInfo(t *testing.T) {
	e := gemsEngine(func(name, path string) (*GemsMember, error) {
		docs := map[string]string{
			"internal versions":  "created_at: x\n---\nrack 9.0.0 11\n",
			"rubygems versions":  "created_at: y\n---\nrack 3.0.0 22\nrails 7.0.0 33\n",
			"internal info/rack": "---\n9.0.0 |checksum:a\n",
			"rubygems info/rack": "---\n3.0.0 |checksum:b\n",
		}
		if d, ok := docs[name+" "+path]; ok {
			return &GemsMember{RemoteName: name, Body: []byte(d)}, nil
		}
		return &GemsMember{RemoteName: name}, nil
	})
	virt := gemsVirt("internal", "rubygems")
	info, ct, err := e.FetchGems(context.Background(), virt, "info/rack")
	if err != nil || !strings.HasPrefix(ct, "text/plain") {
		t.Fatalf("info: %v %q", err, ct)
	}
	versions, _, err := e.FetchGems(context.Background(), virt, "versions")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(versions), "\nrails 7.0.0 33\nrack 9.0.0,3.0.0 "+gems.MD5Hex(info)+"\n") {
		t.Fatalf("versions = %q", versions)
	}
	if _, _, err := e.FetchGems(context.Background(), virt, "info/../x"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("bad name: %v", err)
	}
}
