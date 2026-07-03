package virtual

import (
	"strings"
	"testing"

	"github.com/IUCLabs/artifactapi/pkg/models"
)

func TestRegisterGetMerger(t *testing.T) {
	if _, err := GetMerger(models.PackageHelm); err != nil {
		t.Errorf("helm merger should be registered: %v", err)
	}
	if _, err := GetMerger(models.PackagePyPI); err != nil {
		t.Errorf("pypi merger should be registered: %v", err)
	}
	if _, err := GetMerger(models.PackageType("nope")); err == nil {
		t.Error("expected error for unknown merger")
	}
}

func TestPyPIMerge(t *testing.T) {
	m := &PyPIMerger{}
	members := []MemberIndex{
		{RemoteName: "a", RepoType: models.RepoTypeRemote, Body: []byte(`<a href="pkg/foo-1.0.whl">foo-1.0.whl</a>`)},
		{RemoteName: "b", RepoType: models.RepoTypeLocal, Body: []byte(`<a href="/bar-2.0.whl">bar-2.0.whl</a>`)},
	}
	out, err := m.MergeIndexes(members, "http://proxy")
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	if !strings.Contains(s, "foo-1.0.whl") || !strings.Contains(s, "bar-2.0.whl") {
		t.Errorf("merged index missing entries: %s", s)
	}
	if !strings.Contains(s, "http://proxy/api/v1/remote/a/pkg/foo-1.0.whl") {
		t.Errorf("remote href not rewritten: %s", s)
	}
	if !strings.Contains(s, "http://proxy/api/v1/local/b/bar-2.0.whl") {
		t.Errorf("local href not rewritten: %s", s)
	}

	// Sorted output: foo before... entries sorted by link text.
	if strings.Index(s, "bar-2.0.whl") > strings.Index(s, "foo-1.0.whl") {
		t.Error("entries should be sorted by text")
	}

	// Duplicate link texts across members are de-duplicated.
	dup := []MemberIndex{
		{RemoteName: "a", Body: []byte(`<a href="x">dup</a>`)},
		{RemoteName: "b", Body: []byte(`<a href="y">dup</a>`)},
	}
	out, _ = m.MergeIndexes(dup, "")
	if strings.Count(string(out), ">dup</a>") != 1 {
		t.Errorf("duplicate not de-duplicated: %s", out)
	}
}

func TestPyPIMergeNoProxyAndBadLinks(t *testing.T) {
	m := &PyPIMerger{}
	members := []MemberIndex{{
		RemoteName: "a",
		Body:       []byte("<a href=\"foo.whl\">foo.whl</a>\n<a>no href</a>\n<span>not a link</span>"),
	}}
	// No proxy base URL: hrefs are left as-is.
	out, err := m.MergeIndexes(members, "")
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	if !strings.Contains(s, ">foo.whl</a>") {
		t.Errorf("missing link: %s", s)
	}
}

func TestHelmMerge(t *testing.T) {
	m := &HelmMerger{}
	memberA := `apiVersion: v1
entries:
  alpha:
    - name: alpha
      version: 1.0.0
      urls:
        - charts/alpha-1.0.0.tgz
`
	memberB := `apiVersion: v1
entries:
  beta:
    - name: beta
      version: 2.0.0
      urls:
        - https://charts.example.com/beta-2.0.0.tgz
  gamma:
    - name: gamma
      version: 3.0.0
      urls:
        - https://other-host.example.net/gamma-3.0.0.tgz
`
	members := []MemberIndex{
		{RemoteName: "a", RepoType: models.RepoTypeLocal, BaseURL: "https://charts.example.com", Body: []byte(memberA)},
		{RemoteName: "b", RepoType: models.RepoTypeRemote, BaseURL: "https://charts.example.com", Body: []byte(memberB)},
	}
	out, err := m.MergeIndexes(members, "http://proxy")
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	for _, chart := range []string{"alpha", "beta", "gamma"} {
		if !strings.Contains(s, chart) {
			t.Errorf("merged index missing chart %q: %s", chart, s)
		}
	}
	// Relative URL from a local member is rewritten under /local/.
	if !strings.Contains(s, "http://proxy/api/v1/local/a/charts/alpha-1.0.0.tgz") {
		t.Errorf("relative local url not rewritten: %s", s)
	}
	// Same-host absolute URL from a remote member is rewritten under /remote/.
	if !strings.Contains(s, "http://proxy/api/v1/remote/b/beta-2.0.0.tgz") {
		t.Errorf("same-host absolute url not rewritten: %s", s)
	}
	// Cross-host absolute URL is left untouched.
	if !strings.Contains(s, "https://other-host.example.net/gamma-3.0.0.tgz") {
		t.Errorf("cross-host url should be preserved: %s", s)
	}
}

func TestHelmMergeDedup(t *testing.T) {
	m := &HelmMerger{}
	body := `apiVersion: v1
entries:
  alpha:
    - name: alpha
      version: 1.0.0
      urls: [charts/alpha-1.0.0.tgz]
`
	members := []MemberIndex{
		{RemoteName: "a", BaseURL: "https://x", Body: []byte(body)},
		{RemoteName: "b", BaseURL: "https://x", Body: []byte(body)},
	}
	out, _ := m.MergeIndexes(members, "")
	if strings.Count(string(out), "version: 1.0.0") != 1 {
		t.Errorf("duplicate chart version not de-duplicated: %s", out)
	}
}

func TestHelmMergeInvalidYAML(t *testing.T) {
	m := &HelmMerger{}
	out, err := m.MergeIndexes([]MemberIndex{{RemoteName: "a", Body: []byte("::: not yaml :::")}}, "")
	if err != nil {
		t.Fatalf("invalid member yaml should be skipped, not error: %v", err)
	}
	if !strings.Contains(string(out), "apiVersion") {
		t.Errorf("expected a valid empty merged index: %s", out)
	}
}
