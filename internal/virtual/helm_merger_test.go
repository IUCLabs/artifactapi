package virtual_test

import (
	"strings"
	"testing"

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
