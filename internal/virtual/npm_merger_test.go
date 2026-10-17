package virtual

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/IUCLabs/artifactapi/pkg/models"
)

type npmDoc struct {
	Name     string                    `json:"name"`
	Readme   string                    `json:"readme,omitempty"`
	Modified string                    `json:"modified,omitempty"`
	DistTags map[string]string         `json:"dist-tags"`
	Versions map[string]map[string]any `json:"versions"`
	Time     map[string]string         `json:"time,omitempty"`
}

func npmVersion(name, v, tarball string) map[string]any {
	return map[string]any{"name": name, "version": v, "readme": "long", "scripts": map[string]string{"test": "x"},
		"dependencies": map[string]string{"dep": "^1"}, "dist": map[string]any{"tarball": tarball, "shasum": "abc"}}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func decodeNPM(t *testing.T, b []byte) npmDoc {
	t.Helper()
	var d npmDoc
	if err := json.Unmarshal(b, &d); err != nil {
		t.Fatalf("decode %s: %v", b, err)
	}
	return d
}

func tarballOf(d npmDoc, v string) string {
	dist, _ := d.Versions[v]["dist"].(map[string]any)
	s, _ := dist["tarball"].(string)
	return s
}

func npmMembersFixture(t *testing.T) []NPMMember {
	local := npmDoc{Name: "pkg", DistTags: map[string]string{"latest": "1.0.0", "beta": "9.9.9"},
		Versions: map[string]map[string]any{"1.0.0": npmVersion("pkg", "1.0.0", "pkg/-/pkg-1.0.0.tgz")}}
	remote := npmDoc{Name: "pkg", Readme: "remote readme",
		DistTags: map[string]string{"latest": "2.0.0", "next": "2.0.0", "beta": "1.0.0"},
		Versions: map[string]map[string]any{
			"1.0.0": npmVersion("pkg", "1.0.0", "https://registry.npmjs.org/pkg/-/pkg-1.0.0.tgz"),
			"2.0.0": npmVersion("pkg", "2.0.0", "https://registry.npmjs.org/pkg/-/pkg-2.0.0.tgz"),
			"3.0.0": npmVersion("pkg", "3.0.0", "https://cdn.example/pkg-3.0.0.tgz"),
		},
		Time: map[string]string{"modified": "2024-01-01T00:00:00.000Z", "2.0.0": "2023-01-01T00:00:00.000Z"}}
	return []NPMMember{
		{RemoteName: "internal", Doc: mustJSON(t, local)},
		{RemoteName: "absent"},
		{RemoteName: "npmjs", Bases: []string{"https://registry.npmjs.org/"}, Doc: mustJSON(t, remote)},
	}
}

func TestMergeNPMUnionsVersionsMemberOrderWins(t *testing.T) {
	p, err := MergeNPM(npmMembersFixture(t), "http://aa/api/v1/virtual/npm/")
	if err != nil {
		t.Fatal(err)
	}
	d := decodeNPM(t, p.Full)
	if len(d.Versions) != 3 {
		t.Fatalf("want 3 merged versions, got %v", d.Versions)
	}
	for v, want := range map[string]string{
		"1.0.0": "http://aa/api/v1/virtual/npm/internal/pkg/-/pkg-1.0.0.tgz",
		"2.0.0": "http://aa/api/v1/virtual/npm/npmjs/pkg/-/pkg-2.0.0.tgz",
		"3.0.0": "https://cdn.example/pkg-3.0.0.tgz",
	} {
		if got := tarballOf(d, v); got != want {
			t.Errorf("%s tarball = %q, want %q", v, got, want)
		}
	}
	if d.Readme != "" {
		t.Errorf("top-level fields come from the first member with the package, got readme %q", d.Readme)
	}
	if d.Time["modified"] != "2024-01-01T00:00:00.000Z" || d.Time["2.0.0"] == "" {
		t.Errorf("time not merged: %v", d.Time)
	}
}

func TestMergeNPMDistTags(t *testing.T) {
	members := npmMembersFixture(t)
	members[0].Doc = mustJSON(t, npmDoc{Name: "pkg", DistTags: map[string]string{"latest": "1.0.0", "gone": "0.0.1"},
		Versions: map[string]map[string]any{"1.0.0": npmVersion("pkg", "1.0.0", "pkg/-/pkg-1.0.0.tgz")}})
	p, err := MergeNPM(members, "http://aa/v/")
	if err != nil {
		t.Fatal(err)
	}
	got := decodeNPM(t, p.Full).DistTags
	want := map[string]string{"latest": "1.0.0", "next": "2.0.0", "beta": "1.0.0"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("dist-tags = %v, want %v", got, want)
	}
}

func TestMergeNPMDropsFirstTagNamingMissingVersion(t *testing.T) {
	// internal claims beta=9.9.9 first; it wins over npmjs's beta, and is then
	// dropped because no member has 9.9.9.
	p, err := MergeNPM(npmMembersFixture(t), "http://aa/v/")
	if err != nil {
		t.Fatal(err)
	}
	if tag, ok := decodeNPM(t, p.Full).DistTags["beta"]; ok {
		t.Fatalf("beta should be dropped, got %q", tag)
	}
}

func TestMergeNPMAbbreviated(t *testing.T) {
	p, err := MergeNPM(npmMembersFixture(t), "http://aa/v/")
	if err != nil {
		t.Fatal(err)
	}
	var a map[string]any
	if err := json.Unmarshal(p.Abbreviated, &a); err != nil {
		t.Fatal(err)
	}
	if a["modified"] != "2024-01-01T00:00:00.000Z" || a["name"] != "pkg" || a["readme"] != nil || a["time"] != nil {
		t.Fatalf("bad abbreviated top level: %v", a)
	}
	v := a["versions"].(map[string]any)["2.0.0"].(map[string]any)
	if v["readme"] != nil || v["scripts"] != nil {
		t.Errorf("abbreviated version keeps non-install fields: %v", v)
	}
	if v["dependencies"] == nil || v["dist"].(map[string]any)["tarball"] != "http://aa/v/npmjs/pkg/-/pkg-2.0.0.tgz" {
		t.Errorf("abbreviated version lost install fields: %v", v)
	}
}

func TestMergeNPMNoMemberHasPackage(t *testing.T) {
	if _, err := MergeNPM([]NPMMember{{RemoteName: "a"}, {RemoteName: "b"}}, "http://aa/v/"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func TestNPMMemberPath(t *testing.T) {
	for _, c := range []struct {
		tarball string
		bases   []string
		want    string
		ok      bool
	}{
		{"@s/n/-/n-1.0.0.tgz", nil, "@s/n/-/n-1.0.0.tgz", true},
		{"https://evil.example/x.tgz", nil, "", false},
		{"/abs/x.tgz", nil, "", false},
		{"https://r.example/@s/n/-/n-1.0.0.tgz", []string{"https://m.example", "https://r.example"}, "@s/n/-/n-1.0.0.tgz", true},
		{"https://r.example.evil/x.tgz", []string{"https://r.example"}, "", false},
	} {
		got, ok := npmMemberPath(c.tarball, c.bases)
		if ok != c.ok || ok && got != c.want {
			t.Errorf("npmMemberPath(%q, %v) = %q,%v want %q,%v", c.tarball, c.bases, got, ok, c.want, c.ok)
		}
	}
}

func npmEngine(fetch func(name, pkg string) (*NPMMember, error)) *Engine {
	return &Engine{
		mergeTTL: time.Minute,
		getRemote: func(_ context.Context, name string) (*models.Remote, error) {
			return &models.Remote{Name: name, RepoType: models.RepoTypeRemote}, nil
		},
		npmMember: func(_ context.Context, name, pkg string) (*NPMMember, error) { return fetch(name, pkg) },
	}
}

func npmVirt(members ...string) models.Virtual {
	return models.Virtual{Name: "npm", PackageType: models.PackageNPM, Members: members}
}

func TestFetchNPMScopedAndAbbreviated(t *testing.T) {
	var mu sync.Mutex
	var asked []string
	e := npmEngine(func(name, pkg string) (*NPMMember, error) {
		mu.Lock()
		asked = append(asked, pkg)
		mu.Unlock()
		if name == "b" {
			return &NPMMember{RemoteName: name}, nil
		}
		return &NPMMember{RemoteName: name, Doc: mustJSON(t, npmDoc{Name: pkg, DistTags: map[string]string{"latest": "1.0.0"},
			Versions: map[string]map[string]any{"1.0.0": npmVersion(pkg, "1.0.0", pkg+"/-/n-1.0.0.tgz")}})}, nil
	})
	body, ct, err := e.FetchNPM(context.Background(), npmVirt("a", "b"), "@s%2fn", "http://host", true)
	if err != nil {
		t.Fatal(err)
	}
	if ct != NPMAbbreviatedType || !strings.Contains(string(body), `"tarball":"http://host/api/v1/virtual/npm/a/@s/n/-/n-1.0.0.tgz"`) {
		t.Fatalf("ct %q body %s", ct, body)
	}
	if asked[0] != "@s/n" {
		t.Fatalf("member asked for %q, want decoded scoped name", asked[0])
	}
	full, ct, err := e.FetchNPM(context.Background(), npmVirt("a", "b"), "@s%2fn", "http://host", false)
	if err != nil || ct != "application/json" || string(full) == string(body) {
		t.Fatalf("full packument: ct %q err %v", ct, err)
	}
	if len(asked) != 2 {
		t.Fatalf("second fetch should be served from the merge cache, members asked %d times", len(asked))
	}
}

func TestFetchNPMFailsClosed(t *testing.T) {
	e := npmEngine(func(name, pkg string) (*NPMMember, error) {
		if name == "b" {
			return nil, errors.New("upstream down")
		}
		return &NPMMember{RemoteName: name, Doc: []byte(`{"name":"x","versions":{}}`)}, nil
	})
	_, _, err := e.FetchNPM(context.Background(), npmVirt("a", "b"), "x", "http://host", false)
	if err == nil || errors.Is(err, ErrNotFound) {
		t.Fatalf("want upstream error with a member down, got %v", err)
	}
}

func TestFetchNPMNotFound(t *testing.T) {
	e := npmEngine(func(name, _ string) (*NPMMember, error) { return &NPMMember{RemoteName: name}, nil })
	for _, path := range []string{"missing", "-/npm/v1/security/advisories/bulk", "a/../b", "@s"} {
		if _, _, err := e.FetchNPM(context.Background(), npmVirt("a"), path, "http://host", false); !errors.Is(err, ErrNotFound) {
			t.Errorf("%q: want ErrNotFound, got %v", path, err)
		}
	}
}

func TestFetchNPMMemberLocalNotFound(t *testing.T) {
	e := &Engine{getRemote: func(_ context.Context, name string) (*models.Remote, error) {
		return &models.Remote{Name: name, RepoType: models.RepoTypeLocal}, nil
	}}
	e.NPMLocal = func(_ context.Context, _, pkg string) ([]byte, error) {
		if pkg == "boom" {
			return nil, errors.New("db down")
		}
		return nil, fmt.Errorf("wrapped: %w", ErrNotFound)
	}
	m, err := e.fetchNPMMember(context.Background(), "internal", "nope")
	if err != nil || m.Doc != nil {
		t.Fatalf("missing package must be an empty member, got %v %v", m, err)
	}
	if _, err := e.fetchNPMMember(context.Background(), "internal", "boom"); err == nil {
		t.Fatal("local read errors must surface")
	}
}

func TestMemberRedirectNPM(t *testing.T) {
	e := npmEngine(nil)
	virt := npmVirt("internal", "npmjs")
	loc, ok, err := e.MemberRedirect(context.Background(), virt, "npmjs/@s/n/-/n-1.0.0.tgz", "", "http://host")
	if err != nil || !ok || loc != "http://host/api/v1/remote/npmjs/@s/n/-/n-1.0.0.tgz" {
		t.Fatalf("got %q %v %v", loc, ok, err)
	}
	if _, ok, _ := e.MemberRedirect(context.Background(), virt, "@s/n", "", "http://host"); ok {
		t.Fatal("scoped packument path must not redirect")
	}
	if _, _, err := e.MemberRedirect(context.Background(), virt, "npmjs/%2e%2e/x.tgz", "", "http://host"); !errors.Is(err, ErrBadPath) {
		t.Fatalf("want ErrBadPath, got %v", err)
	}
}

func TestEvictOldestBoundsNPMMerges(t *testing.T) {
	e := npmEngine(func(name, pkg string) (*NPMMember, error) {
		return &NPMMember{RemoteName: name, Doc: mustJSON(t, npmDoc{Name: pkg, Versions: map[string]map[string]any{}})}, nil
	})
	for i := range maxNPMPackuments + 5 {
		if _, _, err := e.FetchNPM(context.Background(), npmVirt("a"), fmt.Sprintf("p%d", i), "http://host", false); err != nil {
			t.Fatal(err)
		}
	}
	if len(e.gens) != maxNPMPackuments {
		t.Fatalf("cached merges = %d, want %d", len(e.gens), maxNPMPackuments)
	}
	if _, ok := e.gens["npm\x00npm\x00p0\x00http://host"]; ok {
		t.Fatal("oldest merge should be evicted")
	}
}
