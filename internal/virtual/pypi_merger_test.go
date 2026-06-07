package virtual_test

import (
	"strings"
	"testing"

	"github.com/IUCLabs/artifactapi/internal/virtual"
)

func TestPyPIMerger_BasicMerge(t *testing.T) {
	m := &virtual.PyPIMerger{}

	member1 := virtual.MemberIndex{
		RemoteName: "pypi-a",
		Body: []byte(`<!DOCTYPE html>
<html><body>
  <a href="/simple/requests/">requests</a>
  <a href="/simple/flask/">flask</a>
</body></html>`),
	}

	member2 := virtual.MemberIndex{
		RemoteName: "pypi-b",
		Body: []byte(`<!DOCTYPE html>
<html><body>
  <a href="/simple/django/">django</a>
</body></html>`),
	}

	result, err := m.MergeIndexes([]virtual.MemberIndex{member1, member2}, "https://proxy.example.com")
	if err != nil {
		t.Fatal(err)
	}

	body := string(result)
	if !strings.Contains(body, "requests") {
		t.Error("expected requests")
	}
	if !strings.Contains(body, "flask") {
		t.Error("expected flask")
	}
	if !strings.Contains(body, "django") {
		t.Error("expected django")
	}
	if !strings.Contains(body, "proxy.example.com/api/v1/remote/pypi-a") {
		t.Error("expected proxy URL for pypi-a")
	}
}

func TestPyPIMerger_Dedup(t *testing.T) {
	m := &virtual.PyPIMerger{}

	idx := []byte(`<html><body>
  <a href="/simple/requests/">requests</a>
</body></html>`)

	members := []virtual.MemberIndex{
		{RemoteName: "a", Body: idx},
		{RemoteName: "b", Body: idx},
	}

	result, err := m.MergeIndexes(members, "")
	if err != nil {
		t.Fatal(err)
	}

	count := strings.Count(string(result), "<a ")
	if count != 1 {
		t.Errorf("expected 1 <a> tag for deduplicated requests, got %d\n%s", count, result)
	}
}

func TestPyPIMerger_Sorted(t *testing.T) {
	m := &virtual.PyPIMerger{}

	member := virtual.MemberIndex{
		RemoteName: "pypi",
		Body: []byte(`<html><body>
  <a href="/z/">zebra</a>
  <a href="/a/">alpha</a>
  <a href="/m/">middle</a>
</body></html>`),
	}

	result, err := m.MergeIndexes([]virtual.MemberIndex{member}, "")
	if err != nil {
		t.Fatal(err)
	}

	body := string(result)
	alphaIdx := strings.Index(body, "alpha")
	middleIdx := strings.Index(body, "middle")
	zebraIdx := strings.Index(body, "zebra")

	if alphaIdx > middleIdx || middleIdx > zebraIdx {
		t.Error("expected sorted output")
	}
}
