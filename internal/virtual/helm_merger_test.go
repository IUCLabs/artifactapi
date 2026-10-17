package virtual_test

import (
	"fmt"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/IUCLabs/artifactapi/internal/virtual"
)

func TestHelmMerger_BasicMerge(t *testing.T) {
	m := &virtual.HelmMerger{}

	member1 := virtual.MemberIndex{
		RemoteName: "repo-a",
		Body: []byte(`apiVersion: v1
entries:
  nginx:
    - name: nginx
      version: "1.0.0"
      urls:
        - https://charts-a.example.com/nginx-1.0.0.tgz
`),
	}

	member2 := virtual.MemberIndex{
		RemoteName: "repo-b",
		Body: []byte(`apiVersion: v1
entries:
  redis:
    - name: redis
      version: "2.0.0"
      urls:
        - https://charts-b.example.com/redis-2.0.0.tgz
`),
	}

	result, err := m.MergeIndexes([]virtual.MemberIndex{member1, member2}, "https://proxy.example.com")
	if err != nil {
		t.Fatal(err)
	}

	body := string(result)
	if !strings.Contains(body, "nginx") {
		t.Error("expected nginx in merged index")
	}
	if !strings.Contains(body, "redis") {
		t.Error("expected redis in merged index")
	}
	if !strings.Contains(body, "proxy.example.com/api/v1/remote/repo-a") {
		t.Error("expected proxy URL for repo-a")
	}
	if !strings.Contains(body, "proxy.example.com/api/v1/remote/repo-b") {
		t.Error("expected proxy URL for repo-b")
	}
}

func TestHelmMerger_Dedup(t *testing.T) {
	m := &virtual.HelmMerger{}

	idx := []byte(`apiVersion: v1
entries:
  nginx:
    - name: nginx
      version: "1.0.0"
      urls:
        - nginx-1.0.0.tgz
`)

	members := []virtual.MemberIndex{
		{RemoteName: "repo-a", Body: idx},
		{RemoteName: "repo-b", Body: idx},
	}

	result, err := m.MergeIndexes(members, "")
	if err != nil {
		t.Fatal(err)
	}

	count := strings.Count(string(result), "name: nginx")
	if count != 1 {
		t.Errorf("expected 1 entry for nginx, got %d\n%s", count, result)
	}
}

func TestHelmMerger_PriorityOrder(t *testing.T) {
	m := &virtual.HelmMerger{}

	member1 := virtual.MemberIndex{
		RemoteName: "priority-repo",
		Body: []byte(`apiVersion: v1
entries:
  chart:
    - name: chart
      version: "1.0.0"
      urls:
        - chart-from-priority.tgz
`),
	}

	member2 := virtual.MemberIndex{
		RemoteName: "fallback-repo",
		Body: []byte(`apiVersion: v1
entries:
  chart:
    - name: chart
      version: "1.0.0"
      urls:
        - chart-from-fallback.tgz
`),
	}

	result, err := m.MergeIndexes([]virtual.MemberIndex{member1, member2}, "https://proxy")
	if err != nil {
		t.Fatal(err)
	}

	body := string(result)
	if !strings.Contains(body, "priority-repo") {
		t.Error("expected priority repo URL to win")
	}
	if strings.Contains(body, "fallback-repo") {
		t.Error("expected fallback repo to be excluded for duplicate")
	}
}

func TestHelmMerger_PreservesChartFields(t *testing.T) {
	m := &virtual.HelmMerger{}
	body := []byte(`apiVersion: v1
entries:
  app:
    - apiVersion: v2
      name: app
      version: "1.0"
      appVersion: "3.1"
      description: in-house
      digest: deadbeef
      urls:
        - app-1.0.tgz
`)
	result, err := m.MergeIndexes([]virtual.MemberIndex{{RemoteName: "charts", RepoType: "local", Body: body}}, "https://proxy.example.com")
	if err != nil {
		t.Fatal(err)
	}
	out := string(result)
	for _, want := range []string{
		"digest: deadbeef",
		"apiVersion: v2",
		`appVersion: "3.1"`,
		"description: in-house",
		`version: "1.0"`,
		"https://proxy.example.com/api/v1/local/charts/app-1.0.tgz",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("merged index missing %q:\n%s", want, out)
		}
	}
}

func TestHelmMerger_ExpandsAliasesAcrossDedupe(t *testing.T) {
	m := &virtual.HelmMerger{}
	first := []byte(`apiVersion: v1
entries:
  app:
    - version: "1.0"
      urls: [app-1.0.tgz]
`)
	second := []byte(`apiVersion: v1
entries:
  app:
    - version: "1.0"
      maintainers: &team
        - name: ops
      urls: [app-1.0.tgz]
    - version: "2.0"
      maintainers: *team
      urls: [app-2.0.tgz]
`)
	result, err := m.MergeIndexes([]virtual.MemberIndex{
		{RemoteName: "a", RepoType: "local", Body: first},
		{RemoteName: "b", RepoType: "local", Body: second},
	}, "https://proxy.example.com")
	if err != nil {
		t.Fatal(err)
	}
	var idx struct {
		Entries map[string][]struct {
			Version     string `yaml:"version"`
			Maintainers []struct {
				Name string `yaml:"name"`
			} `yaml:"maintainers"`
		} `yaml:"entries"`
	}
	if err := yaml.Unmarshal(result, &idx); err != nil {
		t.Fatalf("merged index does not parse: %v\n%s", err, result)
	}
	app := idx.Entries["app"]
	if len(app) != 2 || app[1].Version != "2.0" || len(app[1].Maintainers) != 1 || app[1].Maintainers[0].Name != "ops" {
		t.Errorf("alias not expanded: %+v\n%s", app, result)
	}
}

func TestHelmMerger_DropsAliasBomb(t *testing.T) {
	m := &virtual.HelmMerger{}
	body := []byte(`apiVersion: v1
entries:
  app:
    - version: "1.0"
      a: &a [x, x, x, x, x, x, x, x, x, x]
      b: &b [*a, *a, *a, *a, *a, *a, *a, *a, *a, *a]
      c: &c [*b, *b, *b, *b, *b, *b, *b, *b, *b, *b]
      d: &d [*c, *c, *c, *c, *c, *c, *c, *c, *c, *c]
      e: &e [*d, *d, *d, *d, *d, *d, *d, *d, *d, *d]
      urls: [app-1.0.tgz]
`)
	result, err := m.MergeIndexes([]virtual.MemberIndex{{RemoteName: "a", RepoType: "local", Body: body}}, "https://proxy.example.com")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(result), "app-1.0.tgz") {
		t.Errorf("alias bomb entry was kept")
	}
}

func TestHelmMerger_LargeIndexWithoutAliasesIsComplete(t *testing.T) {
	var b strings.Builder
	b.WriteString("apiVersion: v1\nentries:\n")
	for c := 0; c < 100; c++ {
		fmt.Fprintf(&b, "  chart%d:\n", c)
		for v := 0; v < 100; v++ {
			fmt.Fprintf(&b, "    - name: chart%d\n      version: \"1.0.%d\"\n      description: d\n      maintainers: [{name: ops, email: ops@example.com}]\n      urls: [chart%d-1.0.%d.tgz]\n", c, v, c, v)
		}
	}
	result, err := (&virtual.HelmMerger{}).MergeIndexes([]virtual.MemberIndex{{RemoteName: "a", RepoType: "local", Body: []byte(b.String())}}, "https://proxy.example.com")
	if err != nil {
		t.Fatal(err)
	}
	var idx struct {
		Entries map[string][]map[string]any `yaml:"entries"`
	}
	if err := yaml.Unmarshal(result, &idx); err != nil {
		t.Fatal(err)
	}
	total := 0
	for _, vs := range idx.Entries {
		total += len(vs)
	}
	if len(idx.Entries) != 100 || total != 10000 {
		t.Errorf("got %d charts, %d versions; want 100, 10000", len(idx.Entries), total)
	}
}
