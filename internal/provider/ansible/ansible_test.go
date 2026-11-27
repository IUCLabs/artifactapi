package ansible

import (
	"encoding/json"
	"testing"

	"github.com/IUCLabs/artifactapi/internal/provider"
	"github.com/IUCLabs/artifactapi/pkg/models"
)

const tarball = "api/v3/plugin/ansible/content/published/collections/artifacts/ansible-posix-2.0.0.tar.gz"

func TestClassify(t *testing.T) {
	p := &Provider{}
	tests := []struct {
		path string
		want provider.Mutability
	}{
		{"api/", provider.Mutable},
		{"api/v3/collections/community/general/versions/?limit=100", provider.Mutable},
		{"api/v3/collections/community/general/versions/?x=.tar.gz", provider.Mutable},
		{"api/v3/collections/ansible/posix/versions/2.0.0/", provider.Mutable},
		{"api/v3/collections/ansible/posix/readme.tar.gz", provider.Mutable},
		{tarball, provider.Immutable},
		{tarball + "?limit=1", provider.Immutable},
	}
	for _, tt := range tests {
		if got := p.Classify(tt.path); got != tt.want {
			t.Errorf("Classify(%q) = %v, want %v", tt.path, got, tt.want)
		}
	}
}

func TestContentTypeIgnoresQuery(t *testing.T) {
	if got := (&Provider{}).ContentType("api/v3/collections/a/b/versions/?x=.tar.gz"); got != "application/json" {
		t.Errorf("ContentType = %q, want application/json", got)
	}
}

func TestForwardedQuery(t *testing.T) {
	p := &Provider{}
	if got := p.ForwardedQuery(tarball); got != nil {
		t.Errorf("tarball ForwardedQuery = %v, want nil", got)
	}
	q := p.ForwardedQuery("api/v3/collections/a/b/versions/")
	if len(q) != 2 || q["limit"] == nil || q["offset"] == nil {
		t.Fatalf("metadata ForwardedQuery keys = %v, want limit and offset", q)
	}
	tests := []struct {
		param, in, want string
		ok              bool
	}{
		{"limit", "10", "10", true},
		{"limit", "007", "7", true},
		{"limit", "100", "100", true},
		{"limit", "5000", "100", true},
		{"limit", "0", "", false},
		{"limit", "", "", false},
		{"limit", "-1", "", false},
		{"limit", "+5", "", false},
		{"limit", "1e3", "", false},
		{"limit", "10 ", "", false},
		{"offset", "0", "0", true},
		{"offset", "000", "0", true},
		{"offset", "0100", "100", true},
		{"offset", "abc", "", false},
		{"offset", "99999999999999999999", "", false},
	}
	for _, tt := range tests {
		got, ok := q[tt.param](tt.in)
		if ok != tt.ok || (ok && got != tt.want) {
			t.Errorf("%s(%q) = %q, %v, want %q, %v", tt.param, tt.in, got, ok, tt.want, tt.ok)
		}
	}
}

func rewrite(t *testing.T, remote models.Remote, body string) map[string]any {
	t.Helper()
	out, err := (&Provider{}).RewriteResponse([]byte(body), remote)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("rewritten body is not JSON: %v: %s", err, out)
	}
	return got
}

func TestRewriteResponse(t *testing.T) {
	remote := models.Remote{Name: "galaxy", BaseURL: "https://galaxy.ansible.com/"}
	got := rewrite(t, remote, `{
		"links": {"next": "/api/v3/collections/a/b/versions/?limit=10&offset=10", "previous": null},
		"data": [{
			"href": "/api/v3/collections/a/b/versions/1.0.0/",
			"download_url": "https://galaxy.ansible.com/api/v3/plugin/ansible/content/published/collections/artifacts/a-b-1.0.0.tar.gz",
			"description": "/api/ is where it lives",
			"docs": "/api/v3/not-a-link/",
			"metadata": {"repository": "https://github.com/a/b", "homepage": "https://galaxy.ansible.com/ui/"}
		}]
	}`)
	pre := "/api/v1/remote/galaxy/"
	links := got["links"].(map[string]any)
	if want := pre + "api/v3/collections/a/b/versions/?limit=10&offset=10"; links["next"] != want {
		t.Errorf("links.next = %v, want %q", links["next"], want)
	}
	if links["previous"] != nil {
		t.Errorf("links.previous = %v, want null", links["previous"])
	}
	item := got["data"].([]any)[0].(map[string]any)
	if want := pre + "api/v3/collections/a/b/versions/1.0.0/"; item["href"] != want {
		t.Errorf("href = %v, want %q", item["href"], want)
	}
	if want := pre + "api/v3/plugin/ansible/content/published/collections/artifacts/a-b-1.0.0.tar.gz"; item["download_url"] != want {
		t.Errorf("download_url = %v, want %q", item["download_url"], want)
	}
	for k, want := range map[string]string{"description": "/api/ is where it lives", "docs": "/api/v3/not-a-link/"} {
		if item[k] != want {
			t.Errorf("%s rewritten to %v", k, item[k])
		}
	}
	meta := item["metadata"].(map[string]any)
	if meta["repository"] != "https://github.com/a/b" || meta["homepage"] != "https://galaxy.ansible.com/ui/" {
		t.Errorf("unrelated URLs rewritten: %v", meta)
	}
}

func TestRewriteResponseEscapesRemoteName(t *testing.T) {
	got := rewrite(t, models.Remote{Name: `a"b/c`, BaseURL: "https://g/"}, `{"href":"/api/x/"}`)
	if want := "/api/v1/remote/a%22b%2Fc/api/x/"; got["href"] != want {
		t.Errorf("href = %v, want %q", got["href"], want)
	}
}

func TestRewriteResponseKeepsLargeNumbers(t *testing.T) {
	out, err := (&Provider{}).RewriteResponse([]byte(`{"n":12345678901234567890}`), models.Remote{Name: "g"})
	if err != nil {
		t.Fatal(err)
	}
	if want := "{\"n\":12345678901234567890}\n"; string(out) != want {
		t.Errorf("got %q, want %q", out, want)
	}
}

func TestRewriteResponseSkipsNonJSON(t *testing.T) {
	out, err := (&Provider{}).RewriteResponse([]byte("<html>"), models.Remote{Name: "g"})
	if err != nil || out != nil {
		t.Errorf("got %q, %v; want nil, nil", out, err)
	}
}
