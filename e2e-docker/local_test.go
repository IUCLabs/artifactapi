//go:build dockere2e

package e2edocker

import (
	"bytes"
	"net/http"
	"strings"
	"testing"
	"time"
)

func uploadFile(t *testing.T, repo, filePath string, body []byte, contentType string) {
	t.Helper()
	url := api("/api/v2/remotes/" + repo + "/files/" + filePath)
	resp, respBody := doRequest(t, http.MethodPut, url, body, contentType)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("upload %s: status %d: %s", filePath, resp.StatusCode, respBody)
	}
}

// TestLocalGenericUpload uploads a generic file and downloads it back.
func TestLocalGenericUpload(t *testing.T) {
	createRepo(t, `{"name":"local-generic","package_type":"generic","repo_type":"local"}`)
	defer deleteRepo(t, "local-generic")

	content := []byte("artifactapi local generic upload payload")
	uploadFile(t, "local-generic", "data/hello.bin", content, "application/octet-stream")

	resp, body := doRequest(t, http.MethodGet, api("/api/v1/local/local-generic/data/hello.bin"), nil, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("download: status %d: %s", resp.StatusCode, body)
	}
	if !bytes.Equal(body, content) {
		t.Fatalf("downloaded content mismatch")
	}
}

// TestLocalPyPIUpload uploads a wheel and validates the generated simple index.
func TestLocalPyPIUpload(t *testing.T) {
	createRepo(t, `{"name":"local-pypi","package_type":"pypi","repo_type":"local"}`)
	defer deleteRepo(t, "local-pypi")

	wheel := fixtureBytes(t, "packages/foo-1.0-py3-none-any.whl")
	uploadFile(t, "local-pypi", "foo-1.0-py3-none-any.whl", wheel, "application/zip")

	// Root index lists the package.
	resp, body := doRequest(t, http.MethodGet, api("/api/v1/local/local-pypi/simple/"), nil, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("simple index: status %d: %s", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), "foo") {
		t.Fatalf("simple index missing package 'foo': %s", body)
	}

	// Per-package index lists the wheel file.
	resp, body = doRequest(t, http.MethodGet, api("/api/v1/local/local-pypi/simple/foo/"), nil, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("package index: status %d: %s", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), "foo-1.0-py3-none-any.whl") {
		t.Fatalf("package index missing wheel: %s", body)
	}

	// The wheel downloads back byte-identical.
	resp, body = doRequest(t, http.MethodGet, api("/api/v1/local/local-pypi/foo/foo-1.0-py3-none-any.whl"), nil, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("download wheel: status %d: %s", resp.StatusCode, body)
	}
	if !bytes.Equal(body, wheel) {
		t.Fatalf("wheel content mismatch")
	}
}

// TestLocalRPMRepodata uploads a real RPM and validates that repodata is
// generated automatically (the special rpm-local feature).
func TestLocalRPMRepodata(t *testing.T) {
	createRepo(t, `{"name":"local-rpm","package_type":"rpm","repo_type":"local"}`)
	defer deleteRepo(t, "local-rpm")

	rpm := fixtureBytes(t, "rpmrepo/Packages/e2e-testpkg-1.0-1.noarch.rpm")
	uploadFile(t, "local-rpm", "e2e-testpkg-1.0-1.noarch.rpm", rpm, "application/x-rpm")

	// repodata is generated asynchronously after upload; poll for it.
	resp, body := getEventually(t, api("/api/v1/local/local-rpm/repodata/repomd.xml"), 15*time.Second)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("repomd.xml: status %d: %s", resp.StatusCode, body)
	}
	s := string(body)
	if !strings.Contains(s, "<repomd") || !strings.Contains(s, "primary") {
		t.Fatalf("repomd.xml not a valid repodata document: %s", s)
	}
}
