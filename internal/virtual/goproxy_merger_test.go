package virtual

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/IUCLabs/artifactapi/internal/database"
	"github.com/IUCLabs/artifactapi/internal/testsupport"
	"github.com/IUCLabs/artifactapi/pkg/models"
)

func TestMergeGoList(t *testing.T) {
	d, err := MergeGoList([]GoMember{
		{RemoteName: "internal", Body: []byte("v0.2.0\nv0.1.0\n")},
		{RemoteName: "absent"},
		{RemoteName: "goproxy", Body: []byte("v0.1.0 extra\nv1.0.0-rc.1\n\nnot-a-version\nv0.3.0\n")},
	})
	if err != nil {
		t.Fatal(err)
	}
	if want := "v0.1.0\nv0.2.0\nv0.3.0\nv1.0.0-rc.1\n"; string(d.Body) != want {
		t.Fatalf("list = %q, want %q", d.Body, want)
	}
	if d.Latest != "v0.3.0" {
		t.Fatalf("latest = %q, want the highest release over the prerelease", d.Latest)
	}
	for v, owner := range map[string]string{"v0.1.0": "internal", "v0.2.0": "internal", "v0.3.0": "goproxy", "v1.0.0-rc.1": "goproxy"} {
		if d.Owners[v] != owner {
			t.Errorf("owner of %s = %q, want %q", v, d.Owners[v], owner)
		}
	}

	d, _ = MergeGoList([]GoMember{{RemoteName: "a", Body: []byte("v1.0.0-beta\nv1.0.0-alpha\n")}})
	if d.Latest != "v1.0.0-beta" {
		t.Fatalf("latest with only prereleases = %q", d.Latest)
	}
	if d, err := MergeGoList([]GoMember{{RemoteName: "a", Body: []byte{}}}); err != nil || string(d.Body) != "" || d.Latest != "" {
		t.Fatalf("empty list must merge to an empty 200 body: %v", err)
	}
	if _, err := MergeGoList([]GoMember{{RemoteName: "a"}, {RemoteName: "b"}}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func goEngine(docs map[string]string, fail string) (*Engine, map[string]int) {
	var mu sync.Mutex
	asked := map[string]int{}
	return &Engine{
		mergeTTL: time.Minute,
		getRemote: func(_ context.Context, name string) (*models.Remote, error) {
			rt := models.RepoTypeRemote
			if name == "internal" {
				rt = models.RepoTypeLocal
			}
			return &models.Remote{Name: name, RepoType: rt}, nil
		},
		goMember: func(_ context.Context, name, path string) (*GoMember, error) {
			mu.Lock()
			asked[name+" "+path]++
			mu.Unlock()
			if name == fail {
				return nil, errors.New("upstream down")
			}
			if d, ok := docs[name+" "+path]; ok {
				return &GoMember{RemoteName: name, Body: []byte(d)}, nil
			}
			return &GoMember{RemoteName: name}, nil
		},
	}, asked
}

func goVirt() models.Virtual {
	return models.Virtual{Name: "go", PackageType: models.PackageGoProxy, Members: []string{"internal", "goproxy"}}
}

func TestGoProxyRedirectsToFirstOwner(t *testing.T) {
	e, asked := goEngine(map[string]string{
		"internal myorg.example.net/!hello/@v/list":                       "v0.1.0\nv0.2.0\n",
		"goproxy myorg.example.net/!hello/@v/list":                        "v0.2.0\nv0.3.0-rc.1\n",
		"goproxy golang.org/x/text/@v/list":                               "v0.1.0\n",
		"goproxy myorg.example.net/!hello/@v/v0.0.0-20260101-abcdef.info": "{}",
		"goproxy example.com/notags/@v/list":                              "",
		"goproxy example.com/notags/@latest":                              "{}",
	}, "")
	virt := goVirt()
	body, loc, err := e.GoProxy(context.Background(), virt, "myorg.example.net/!hello/@v/list", "")
	if err != nil || loc != "" || string(body) != "v0.1.0\nv0.2.0\nv0.3.0-rc.1\n" {
		t.Fatalf("list = %q %q %v", body, loc, err)
	}
	for path, want := range map[string]string{
		"myorg.example.net/!hello/@v/v0.2.0.zip":                 "/api/v1/local/internal/myorg.example.net/!hello/@v/v0.2.0.zip",
		"myorg.example.net/!hello/@v/v0.3.0-rc.1.mod":            "/api/v1/remote/goproxy/myorg.example.net/!hello/@v/v0.3.0-rc.1.mod",
		"myorg.example.net/!hello/@latest":                       "/api/v1/local/internal/myorg.example.net/!hello/@v/v0.2.0.info",
		"myorg.example.net/!hello/@v/v0.0.0-20260101-abcdef.zip": "/api/v1/remote/goproxy/myorg.example.net/!hello/@v/v0.0.0-20260101-abcdef.zip",
		"golang.org/x/text/@v/v0.1.0.info":                       "/api/v1/remote/goproxy/golang.org/x/text/@v/v0.1.0.info",
		"example.com/notags/@latest":                             "/api/v1/remote/goproxy/example.com/notags/@latest",
	} {
		_, loc, err := e.GoProxy(context.Background(), virt, path, "")
		if err != nil || loc != want {
			t.Errorf("%s -> %q %v, want %q", path, loc, err, want)
		}
	}
	for _, path := range []string{
		"myorg.example.net/!hello/@v/v9.9.9.zip",
		"myorg.example.net/Hello/@v/list",
		"myorg.example.net/!hello/@v/v0.1.0.tar",
		"myorg.example.net/!hello/@v/../v0.1.0.zip",
		"myorg.example.net/!hello",
		"missing.example/mod/@v/list",
		"missing.example/mod/@latest",
	} {
		if _, _, err := e.GoProxy(context.Background(), virt, path, ""); !errors.Is(err, ErrNotFound) {
			t.Errorf("%s: want ErrNotFound, got %v", path, err)
		}
	}
	if n := asked["internal myorg.example.net/!hello/@v/list"]; n != 1 {
		t.Fatalf("merged list must be cached, internal asked %d times", n)
	}
	if n := asked["goproxy missing.example/mod/@v/list"]; n != 1 {
		t.Fatalf("a module no member has must be cached, goproxy asked %d times", n)
	}
}

func TestGoProxyFollowsOwnerChange(t *testing.T) {
	docs := map[string]string{"internal example.com/mod/@v/list": "v0.1.0\n"}
	e, _ := goEngine(docs, "")
	e.mergeTTL = 0
	zip := func() string {
		t.Helper()
		_, loc, err := e.GoProxy(context.Background(), goVirt(), "example.com/mod/@v/v0.1.0.zip", "")
		if err != nil {
			t.Fatal(err)
		}
		return loc
	}
	if loc := zip(); loc != "/api/v1/local/internal/example.com/mod/@v/v0.1.0.zip" {
		t.Fatalf("before move: %q", loc)
	}
	delete(docs, "internal example.com/mod/@v/list")
	docs["goproxy example.com/mod/@v/list"] = "v0.1.0\n"
	if loc := zip(); loc != "/api/v1/remote/goproxy/example.com/mod/@v/v0.1.0.zip" {
		t.Fatalf("same list, new owner: redirect still %q", loc)
	}
}

func TestGoProxyFailsClosed(t *testing.T) {
	e, _ := goEngine(map[string]string{"internal myorg.example.net/hello/@v/list": "v0.1.0\n"}, "goproxy")
	for _, path := range []string{"myorg.example.net/hello/@v/list", "myorg.example.net/hello/@latest", "myorg.example.net/hello/@v/v0.1.0.zip"} {
		if _, _, err := e.GoProxy(context.Background(), goVirt(), path, ""); err == nil || errors.Is(err, ErrNotFound) {
			t.Errorf("%s: want member error, got %v", path, err)
		}
	}
}

func TestGoProxyLocalMemberNotFound(t *testing.T) {
	ctx := context.Background()
	dsn, terminate, err := testsupport.StartPostgres(ctx)
	if err != nil {
		t.Skip("Docker unavailable")
	}
	defer terminate()
	db, err := database.New(dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	local := models.Remote{Name: "internal", PackageType: models.PackageGoProxy, RepoType: models.RepoTypeLocal}
	if err := db.CreateRemote(ctx, &local); err != nil {
		t.Fatal(err)
	}

	e := NewEngine(db, nil)
	if _, err := e.fetchMemberPath(ctx, local, "example.com/mod/@latest"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("local 404 must be ErrNotFound, got %v", err)
	}
	virt := models.Virtual{Name: "go", PackageType: models.PackageGoProxy, Members: []string{"internal"}}
	if _, _, err := e.GoProxy(ctx, virt, "example.com/mod/@latest", ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("module only absent from a local member must be not-found, got %v", err)
	}
}
