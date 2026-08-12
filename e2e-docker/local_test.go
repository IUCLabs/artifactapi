//go:build dockere2e

package e2edocker

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/IUCLabs/artifactapi/internal/testsupport"
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

// TestLocalDebRepo uploads a .deb and validates that the flat apt index
// (Packages / Release) is generated automatically from the parsed control
// stanza (the deb-local analog of rpm repodata generation).
func TestLocalDebRepo(t *testing.T) {
	createRepo(t, `{"name":"local-deb","package_type":"deb","repo_type":"local"}`)
	defer deleteRepo(t, "local-deb")

	deb := testsupport.MinimalDeb("e2e-testpkg", "1.0.0", "amd64")
	uploadFile(t, "local-deb", "e2e-testpkg_1.0.0_amd64.deb", deb, "application/vnd.debian.binary-package")

	// The index is generated asynchronously after upload; poll for it.
	resp, body := getEventually(t, api("/api/v1/local/local-deb/Packages"), 15*time.Second)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("Packages: status %d: %s", resp.StatusCode, body)
	}
	pkgs := string(body)
	for _, want := range []string{"Package: e2e-testpkg", "Version: 1.0.0", "Architecture: amd64", "Filename: pool/e2e-testpkg_1.0.0_amd64.deb", "SHA256:"} {
		if !strings.Contains(pkgs, want) {
			t.Fatalf("Packages missing %q:\n%s", want, pkgs)
		}
	}

	resp, body = doRequest(t, http.MethodGet, api("/api/v1/local/local-deb/Release"), nil, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("Release: status %d: %s", resp.StatusCode, body)
	}
	rel := string(body)
	for _, want := range []string{"Architectures: amd64", "SHA256:", "Packages"} {
		if !strings.Contains(rel, want) {
			t.Fatalf("Release missing %q:\n%s", want, rel)
		}
	}

	// The .deb downloads back byte-identical from its pool path.
	resp, body = doRequest(t, http.MethodGet, api("/api/v1/local/local-deb/pool/e2e-testpkg_1.0.0_amd64.deb"), nil, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("download deb: status %d: %s", resp.StatusCode, body)
	}
	if !bytes.Equal(body, deb) {
		t.Fatalf("deb content mismatch")
	}
}

// TestLocalAlpineIndex uploads an .apk to an alpine local repo and validates
// that a per-arch APKINDEX.tar.gz is generated automatically from the parsed
// .PKGINFO (the apk-local analog of rpm repodata / deb Packages generation).
func TestLocalAlpineIndex(t *testing.T) {
	createRepo(t, `{"name":"local-alpine","package_type":"alpine","repo_type":"local"}`)
	defer deleteRepo(t, "local-alpine")

	apk := testsupport.MinimalApk("e2e-testpkg", "1.0-r0", "x86_64")
	uploadFile(t, "local-alpine", "x86_64/e2e-testpkg-1.0-r0.apk", apk, "application/vnd.android.package-archive")

	// The index is generated asynchronously after upload; poll for it.
	resp, body := getEventually(t, api("/api/v1/local/local-alpine/x86_64/APKINDEX.tar.gz"), 15*time.Second)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("APKINDEX: status %d: %s", resp.StatusCode, body)
	}

	zr, err := gzip.NewReader(bytes.NewReader(body))
	if err != nil {
		t.Fatalf("APKINDEX not gzip: %v", err)
	}
	tarBytes, _ := io.ReadAll(zr)
	tr := tar.NewReader(bytes.NewReader(tarBytes))
	var index string
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("APKINDEX not tar: %v", err)
		}
		if hdr.Name == "APKINDEX" {
			b, _ := io.ReadAll(tr)
			index = string(b)
		}
	}
	for _, want := range []string{"P:e2e-testpkg", "V:1.0-r0", "A:x86_64", "C:Q1", "S:", "I:"} {
		if !strings.Contains(index, want) {
			t.Fatalf("APKINDEX missing %q:\n%s", want, index)
		}
	}

	// The .apk downloads back byte-identical from its arch path.
	resp, body = doRequest(t, http.MethodGet, api("/api/v1/local/local-alpine/x86_64/e2e-testpkg-1.0-r0.apk"), nil, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("download apk: status %d: %s", resp.StatusCode, body)
	}
	if !bytes.Equal(body, apk) {
		t.Fatalf("apk content mismatch")
	}
}
