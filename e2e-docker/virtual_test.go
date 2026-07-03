//go:build dockere2e

package e2edocker

import (
	"net/http"
	"strings"
	"testing"
)

// TestVirtualPyPIMerge uploads different packages to two pypi locals and
// checks that a virtual over them serves a merged simple index.
func TestVirtualPyPIMerge(t *testing.T) {
	createRepo(t, `{"name":"pmerge-a","package_type":"pypi","repo_type":"local"}`)
	createRepo(t, `{"name":"pmerge-b","package_type":"pypi","repo_type":"local"}`)
	defer deleteRepo(t, "pmerge-a")
	defer deleteRepo(t, "pmerge-b")

	uploadFile(t, "pmerge-a", "foo-1.0-py3-none-any.whl", fixtureBytes(t, "packages/foo-1.0-py3-none-any.whl"), "application/zip")
	uploadFile(t, "pmerge-b", "bar-2.0-py3-none-any.whl", []byte("bar wheel payload"), "application/zip")

	createVirtual(t, `{"name":"pmerge-v","package_type":"pypi","members":["pmerge-a","pmerge-b"]}`)
	defer deleteVirtual(t, "pmerge-v")

	resp, body := doRequest(t, http.MethodGet, api("/api/v1/virtual/pmerge-v/simple/"), nil, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("virtual simple index: status %d: %s", resp.StatusCode, body)
	}
	s := string(body)
	if !strings.Contains(s, "foo") || !strings.Contains(s, "bar") {
		t.Fatalf("merged index missing a member package (want foo and bar): %s", s)
	}
}

// TestVirtualHelmMerge points two helm remotes at mock index.yaml documents
// with distinct charts and checks the virtual merges both into one index.
func TestVirtualHelmMerge(t *testing.T) {
	createRepo(t, `{"name":"hmerge-a","package_type":"helm","repo_type":"remote","base_url":"`+mockUpstream()+`/helm-a","stale_on_error":true}`)
	createRepo(t, `{"name":"hmerge-b","package_type":"helm","repo_type":"remote","base_url":"`+mockUpstream()+`/helm-b","stale_on_error":true}`)
	defer deleteRepo(t, "hmerge-a")
	defer deleteRepo(t, "hmerge-b")

	createVirtual(t, `{"name":"hmerge-v","package_type":"helm","members":["hmerge-a","hmerge-b"]}`)
	defer deleteVirtual(t, "hmerge-v")

	resp, body := doRequest(t, http.MethodGet, api("/api/v1/virtual/hmerge-v/index.yaml"), nil, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("virtual index.yaml: status %d: %s", resp.StatusCode, body)
	}
	s := string(body)
	if !strings.Contains(s, "alpha") || !strings.Contains(s, "beta") {
		t.Fatalf("merged helm index missing a member chart (want alpha and beta): %s", s)
	}
}
