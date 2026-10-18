package virtual

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/IUCLabs/artifactapi/internal/provider/ansible"
	"github.com/IUCLabs/artifactapi/internal/proxy"
	"github.com/IUCLabs/artifactapi/pkg/models"
)

func refs(vs ...string) []ansible.VersionRef {
	out := make([]ansible.VersionRef, len(vs))
	for i, v := range vs {
		out[i] = ansible.VersionRef{Version: v}
	}
	return out
}

func TestMergeAnsibleVersionsMemberOrderWins(t *testing.T) {
	d, err := MergeAnsibleVersions([]AnsibleMember{
		{RemoteName: "internal", Found: true, Refs: refs("2.0.0", "1.0.0")},
		{RemoteName: "missing"},
		{RemoteName: "galaxy", Found: true, Refs: refs("3.0.0", "2.0.0", "0.9.0")},
	})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, r := range d.Refs {
		got = append(got, r.Version)
	}
	if strings.Join(got, ",") != "2.0.0,1.0.0,3.0.0,0.9.0" {
		t.Fatalf("merged = %v", got)
	}
	for v, owner := range map[string]string{"2.0.0": "internal", "1.0.0": "internal", "3.0.0": "galaxy", "0.9.0": "galaxy"} {
		if d.Owners[v] != owner {
			t.Errorf("owner of %s = %q, want %q", v, d.Owners[v], owner)
		}
	}
	if _, err := MergeAnsibleVersions([]AnsibleMember{{RemoteName: "a"}, {RemoteName: "b"}}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("no member has it: want ErrNotFound, got %v", err)
	}
	if d, err := MergeAnsibleVersions([]AnsibleMember{{RemoteName: "a", Found: true}}); err != nil || len(d.Refs) != 0 {
		t.Fatalf("found with no versions = %v %v", d, err)
	}
}

// galaxyPages answers a remote's versions list like Galaxy, two per page.
func galaxyPages(versions []string, path string) ([]byte, error) {
	_, q, _ := strings.Cut(path, "?")
	offset := 0
	if _, off, ok := strings.Cut(q, "offset="); ok {
		_, _ = fmt.Sscan(off, &offset)
	}
	end := min(offset+2, len(versions))
	return json.Marshal(map[string]any{"meta": map[string]int{"count": len(versions)}, "data": refs(versions[min(offset, end):end]...)})
}

func ansibleEngine(doc func(remote models.Remote, path string) ([]byte, error)) *Engine {
	return &Engine{
		mergeTTL: time.Minute,
		getRemote: func(_ context.Context, name string) (*models.Remote, error) {
			rt := models.RepoTypeRemote
			if name == "internal" {
				rt = models.RepoTypeLocal
			}
			return &models.Remote{Name: name, RepoType: rt}, nil
		},
		ansibleDoc: func(_ context.Context, remote models.Remote, path string) ([]byte, error) { return doc(remote, path) },
	}
}

func ansibleVirt(members ...string) models.Virtual {
	return models.Virtual{Name: "ansible", PackageType: models.PackageAnsible, Members: members}
}

func TestAnsibleVersionsFollowsRemotePages(t *testing.T) {
	var mu sync.Mutex
	var asked []string
	e := ansibleEngine(func(remote models.Remote, path string) ([]byte, error) {
		mu.Lock()
		asked = append(asked, remote.Name+" "+path)
		mu.Unlock()
		if remote.Name == "internal" {
			return galaxyPages([]string{"1.0.0", "5.0.0"}, "")
		}
		return galaxyPages([]string{"5.0.0", "4.0.0", "3.0.0", "2.0.0", "1.5.0"}, path)
	})
	virt := ansibleVirt("internal", "galaxy")
	d, err := e.AnsibleVersions(context.Background(), virt, "community", "general")
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Refs) != 6 || d.Owners["5.0.0"] != "internal" || d.Owners["1.5.0"] != "galaxy" {
		t.Fatalf("merged = %+v", d)
	}
	want := map[string]bool{
		"internal api/v3/collections/community/general/versions/":                  true,
		"galaxy api/v3/collections/community/general/versions/?limit=100":          true,
		"galaxy api/v3/collections/community/general/versions/?limit=100&offset=2": true,
		"galaxy api/v3/collections/community/general/versions/?limit=100&offset=4": true,
	}
	if len(asked) != len(want) {
		t.Fatalf("asked %v", asked)
	}
	for _, a := range asked {
		if !want[a] {
			t.Errorf("unexpected fetch %q", a)
		}
	}
	if _, err := e.AnsibleVersions(context.Background(), virt, "community", "general"); err != nil || len(asked) != len(want) {
		t.Fatalf("merge not cached: %v, %d fetches", err, len(asked))
	}
}

func TestAnsibleVersionsFailsClosed(t *testing.T) {
	e := ansibleEngine(func(remote models.Remote, path string) ([]byte, error) {
		switch remote.Name {
		case "internal":
			return nil, ErrNotFound
		case "gone":
			return nil, &proxy.ProxyError{Status: http.StatusNotFound}
		}
		return nil, errors.New("upstream down")
	})
	if _, err := e.AnsibleVersions(context.Background(), ansibleVirt("internal", "gone"), "a", "b"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("all members lack it: want ErrNotFound, got %v", err)
	}
	if _, err := e.AnsibleVersions(context.Background(), ansibleVirt("internal", "galaxy"), "a", "b"); err == nil || errors.Is(err, ErrNotFound) {
		t.Fatalf("member error: want a non-404 error, got %v", err)
	}
}

func TestAnsibleVersionsShortRemote(t *testing.T) {
	e := ansibleEngine(func(_ models.Remote, path string) ([]byte, error) {
		if strings.Contains(path, "offset=") {
			return json.Marshal(map[string]any{"meta": map[string]int{"count": 3}, "data": []any{}})
		}
		return galaxyPages([]string{"3.0.0", "2.0.0", "1.0.0"}, path)
	})
	if _, err := e.AnsibleVersions(context.Background(), ansibleVirt("galaxy"), "a", "b"); err == nil || errors.Is(err, ErrNotFound) {
		t.Fatalf("empty page before count: want a non-404 error, got %v", err)
	}
}

func TestAnsibleVersionRoutesViaVirtual(t *testing.T) {
	detail := func(dl string) []byte {
		b, _ := json.Marshal(map[string]any{"version": "1.0.0", "download_url": dl, "artifact": map[string]string{"sha256": "ab"}})
		return b
	}
	dls := map[string]string{
		"internal": "/api/v1/local/internal/artifacts/a-b-1.0.0.tar.gz",
		"galaxy":   "/api/v1/remote/galaxy/api/v3/plugin/ansible/content/published/collections/artifacts/a-b-2.0.0.tar.gz",
		"evil":     "https://evil.example/api/v1/remote/evil/x.tar.gz",
		"other":    "/api/v1/remote/someoneelse/x.tar.gz",
	}
	e := ansibleEngine(func(remote models.Remote, path string) ([]byte, error) {
		if strings.Contains(path, "/versions/?") || strings.HasSuffix(path, "/versions/") {
			v := map[string]string{"internal": "1.0.0", "galaxy": "2.0.0", "evil": "3.0.0", "other": "4.0.0"}[remote.Name]
			return galaxyPages([]string{v}, path)
		}
		return detail(dls[remote.Name]), nil
	})
	virt := ansibleVirt("internal", "galaxy", "evil", "other")
	for v, want := range map[string]string{
		"1.0.0": "https://h/api/v1/virtual/ansible/internal/artifacts/a-b-1.0.0.tar.gz",
		"2.0.0": "https://h/api/v1/virtual/ansible/galaxy/api/v3/plugin/ansible/content/published/collections/artifacts/a-b-2.0.0.tar.gz",
	} {
		body, err := e.AnsibleVersion(context.Background(), virt, "a", "b", v, "https://h")
		if err != nil {
			t.Fatal(err)
		}
		var got struct {
			Href        string
			DownloadURL string `json:"download_url"`
			Artifact    struct{ Sha256 string }
		}
		_ = json.Unmarshal(body, &got)
		if got.DownloadURL != want || got.Href != "https://h/api/v1/virtual/ansible/api/v3/collections/a/b/versions/"+v+"/" || got.Artifact.Sha256 != "ab" {
			t.Errorf("%s detail = %+v", v, got)
		}
		loc, ok, err := e.MemberRedirect(context.Background(), virt, strings.TrimPrefix(want, "https://h/api/v1/virtual/ansible/"), "", "https://h")
		if err != nil || !ok || loc != "https://h"+dls[map[string]string{"1.0.0": "internal", "2.0.0": "galaxy"}[v]] {
			t.Errorf("%s redirect = %q %v %v", v, loc, ok, err)
		}
	}
	for _, v := range []string{"3.0.0", "4.0.0"} {
		if _, err := e.AnsibleVersion(context.Background(), virt, "a", "b", v, "https://h"); err == nil || errors.Is(err, ErrNotFound) {
			t.Errorf("%s: foreign download_url accepted: %v", v, err)
		}
	}
	if _, err := e.AnsibleVersion(context.Background(), virt, "a", "b", "9.9.9", "https://h"); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown version: want ErrNotFound, got %v", err)
	}
	if _, _, err := e.MemberRedirect(context.Background(), virt, "galaxy/../internal/x", "", "https://h"); !errors.Is(err, ErrBadPath) {
		t.Errorf("escaping redirect: want ErrBadPath, got %v", err)
	}
}
