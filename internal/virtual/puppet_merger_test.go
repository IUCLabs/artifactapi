package virtual

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/IUCLabs/artifactapi/internal/proxy"
	"github.com/IUCLabs/artifactapi/pkg/models"
)

func puppetRel(slug, version string) map[string]any {
	return map[string]any{
		"slug": slug + "-" + version, "version": version,
		"file_uri": "/v3/files/" + slug + "-" + version + ".tar.gz",
		"module":   map[string]any{"slug": slug, "uri": "/v3/modules/" + slug},
	}
}

func versionsOf(rs []map[string]any) string {
	var out []string
	for _, r := range rs {
		out = append(out, r["version"].(string))
	}
	return strings.Join(out, ",")
}

func TestMergePuppetReleases(t *testing.T) {
	absolute := puppetRel("myorg-hello", "0.3.0")
	absolute["file_uri"] = "https://forge.example/api/v1/remote/forge/v3/files/myorg-hello-0.3.0.tar.gz"
	internalDup := puppetRel("myorg-hello", "0.2.0")
	internalDup["tag"] = "internal"
	wrongFile := puppetRel("myorg-hello", "9.0.0")
	wrongFile["file_uri"] = "/v3/files/myorg-other-9.0.0.tar.gz"

	d, err := MergePuppetReleases("myorg-hello", []PuppetMember{
		{RemoteName: "internal", Found: true, Releases: []map[string]any{puppetRel("myorg-hello", "0.1.0"), internalDup}},
		{RemoteName: "missing"},
		{RemoteName: "forge", Found: true, Releases: []map[string]any{absolute, puppetRel("myorg-hello", "0.2.0"), puppetRel("myorg-hello", "0.10.0"), wrongFile, puppetRel("myorg-other", "1.0.0")}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := versionsOf(d.Releases); got != "0.10.0,0.3.0,0.2.0,0.1.0" {
		t.Fatalf("merged = %s, want newest semver first without foreign or misnamed releases", got)
	}
	for v, owner := range map[string]string{"0.1.0": "internal", "0.2.0": "internal", "0.3.0": "forge", "0.10.0": "forge"} {
		if d.Owners[v] != owner {
			t.Errorf("owner of %s = %q, want %q", v, d.Owners[v], owner)
		}
	}
	if d.Releases[2]["tag"] != "internal" {
		t.Errorf("duplicate 0.2.0 not taken from the first member: %v", d.Releases[2])
	}
	if d.Releases[1]["file_uri"] != "/v3/files/myorg-hello-0.3.0.tar.gz" {
		t.Errorf("file_uri = %v, want routed via the virtual", d.Releases[1]["file_uri"])
	}

	mod, err := PuppetModule(d)
	if err != nil {
		t.Fatal(err)
	}
	if mod["slug"] != "myorg-hello" || mod["current_release"].(map[string]any)["version"] != "0.10.0" || len(mod["releases"].([]map[string]any)) != 4 {
		t.Fatalf("module = %v", mod)
	}

	if _, err := MergePuppetReleases("myorg-hello", []PuppetMember{{RemoteName: "a"}, {RemoteName: "b"}}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("no member has it: want ErrNotFound, got %v", err)
	}
}

// forgePages answers a member's /v3/releases like the Forge, two per page.
func forgePages(slug string, versions []string, p string) ([]byte, error) {
	_, raw, _ := strings.Cut(p, "?")
	q, _ := url.ParseQuery(raw)
	offset, _ := strconv.Atoi(q.Get("offset"))
	end := min(offset+2, len(versions))
	var results []map[string]any
	for _, v := range versions[min(offset, end):end] {
		results = append(results, puppetRel(slug, v))
	}
	return json.Marshal(map[string]any{"pagination": map[string]int{"total": len(versions)}, "results": results})
}

func puppetEngine(doc func(remote models.Remote, path string) ([]byte, error)) *Engine {
	return &Engine{
		mergeTTL: time.Minute,
		getRemote: func(_ context.Context, name string) (*models.Remote, error) {
			rt := models.RepoTypeRemote
			if name == "internal" {
				rt = models.RepoTypeLocal
			}
			return &models.Remote{Name: name, RepoType: rt}, nil
		},
		puppetDoc: func(_ context.Context, remote models.Remote, path string) ([]byte, error) { return doc(remote, path) },
	}
}

func TestPuppetReleasesFollowsMemberPages(t *testing.T) {
	var mu sync.Mutex
	var asked []string
	e := puppetEngine(func(remote models.Remote, p string) ([]byte, error) {
		mu.Lock()
		asked = append(asked, remote.Name+" "+p)
		mu.Unlock()
		switch remote.Name {
		case "internal":
			return forgePages("puppetlabs-stdlib", []string{"9.6.0"}, p)
		case "gone":
			return nil, &proxy.ProxyError{Status: 404}
		}
		return forgePages("puppetlabs-stdlib", []string{"9.6.0", "9.5.0", "9.4.0", "8.0.0", "7.0.0"}, p)
	})
	virt := models.Virtual{Name: "puppet", PackageType: models.PackagePuppet, Members: []string{"internal", "gone", "forge"}}
	d, err := e.PuppetReleases(context.Background(), virt, "puppetlabs-stdlib")
	if err != nil {
		t.Fatal(err)
	}
	if versionsOf(d.Releases) != "9.6.0,9.5.0,9.4.0,8.0.0,7.0.0" || d.Owners["9.6.0"] != "internal" || d.Owners["7.0.0"] != "forge" {
		t.Fatalf("merged = %s %v", versionsOf(d.Releases), d.Owners)
	}
	if len(asked) != 5 {
		t.Fatalf("want 1 internal + 1 gone + 3 forge pages, asked %v", asked)
	}
	for _, a := range asked {
		if strings.HasPrefix(a, "forge ") && !strings.Contains(a, "module=puppetlabs-stdlib") {
			t.Errorf("member page without module filter: %q", a)
		}
	}
	if _, err := e.PuppetReleases(context.Background(), virt, "puppetlabs-stdlib"); err != nil || len(asked) != 5 {
		t.Fatalf("merge not cached: %v, %d fetches", err, len(asked))
	}
}

func TestPuppetReleasesFailsClosed(t *testing.T) {
	for name, doc := range map[string]func(models.Remote, string) ([]byte, error){
		"member error": func(r models.Remote, p string) ([]byte, error) {
			if r.Name == "forge" {
				return nil, &proxy.ProxyError{Status: 503}
			}
			return forgePages("myorg-hello", []string{"0.1.0"}, p)
		},
		"short member list": func(models.Remote, string) ([]byte, error) {
			return []byte(`{"pagination":{"total":3},"results":[]}`), nil
		},
	} {
		e := puppetEngine(doc)
		virt := models.Virtual{Name: "puppet", PackageType: models.PackagePuppet, Members: []string{"internal", "forge"}}
		if _, err := e.PuppetReleases(context.Background(), virt, "myorg-hello"); err == nil || errors.Is(err, ErrNotFound) {
			t.Errorf("%s: want a non-404 error, got %v", name, err)
		}
	}
}

func TestPuppetRedirect(t *testing.T) {
	e := puppetEngine(func(r models.Remote, p string) ([]byte, error) {
		if r.Name == "internal" {
			return forgePages("myorg-hello", []string{"0.1.0"}, p)
		}
		return forgePages("myorg-hello", []string{"0.2.0", "0.1.0"}, p)
	})
	virt := models.Virtual{Name: "puppet", PackageType: models.PackagePuppet, Members: []string{"internal", "forge"}}
	for p, want := range map[string]string{
		"v3/files/myorg-hello-0.1.0.tar.gz": "https://a.example/api/v1/local/internal/v3/files/myorg-hello-0.1.0.tar.gz",
		"v3/releases/myorg-hello-0.2.0":     "https://a.example/api/v1/remote/forge/v3/releases/myorg-hello-0.2.0",
	} {
		if loc, err := e.PuppetRedirect(context.Background(), virt, p, "https://a.example"); err != nil || loc != want {
			t.Errorf("%s -> %q %v, want %q", p, loc, err, want)
		}
	}
	for _, p := range []string{"v3/files/myorg-hello-0.3.0.tar.gz", "v3/files/myorg-hello-0.1.0.zip", "v3/releases/nope", "v3/users/myorg"} {
		if _, err := e.PuppetRedirect(context.Background(), virt, p, "https://a.example"); !errors.Is(err, ErrNotFound) {
			t.Errorf("%s: want ErrNotFound, got %v", p, err)
		}
	}
}

func TestPuppetReleasesCachesMissingModule(t *testing.T) {
	var mu sync.Mutex
	asked := map[string]int{}
	e := puppetEngine(func(r models.Remote, p string) ([]byte, error) {
		mu.Lock()
		asked[r.Name]++
		mu.Unlock()
		if r.Name == "internal" {
			return nil, &proxy.ProxyError{Status: 404}
		}
		return forgePages("myorg-nope", nil, p)
	})
	virt := models.Virtual{Name: "puppet", PackageType: models.PackagePuppet, Members: []string{"internal", "forge"}}
	for range 2 {
		if _, err := e.PuppetReleases(context.Background(), virt, "myorg-nope"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("want ErrNotFound, got %v", err)
		}
	}
	if asked["internal"] != 1 || asked["forge"] != 1 {
		t.Fatalf("a module no member has must be cached, asked %v", asked)
	}
}
