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

// uploadFile PUTs with Basic credentials, as `curl -u ci:$TOKEN` does.
func uploadFile(t *testing.T, repo, filePath string, body []byte, contentType string) {
	t.Helper()
	url := api("/api/v2/remotes/" + repo + "/files/" + filePath)
	resp, respBody := doRequestAuth(t, http.MethodPut, url, body, contentType, basicAuth(authToken))
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
	assertWriteGated(t, http.MethodPut, api("/api/v2/remotes/local-generic/files/data/other.bin"), content, "application/octet-stream", api("/api/v1/local/local-generic/data/hello.bin"))

	resp, body := doRequest(t, http.MethodGet, api("/api/v1/local/local-generic/data/hello.bin"), nil, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("download: status %d: %s", resp.StatusCode, body)
	}
	if !bytes.Equal(body, content) {
		t.Fatalf("downloaded content mismatch")
	}
}

// TestLocalTerraformUpload uploads a provider zip the way a release pipeline
// does and downloads it back anonymously.
func TestLocalTerraformUpload(t *testing.T) {
	createRepo(t, `{"name":"local-terraform","package_type":"terraform","repo_type":"local"}`)
	defer deleteRepo(t, "local-terraform")

	const path = "myorg/e2e/terraform-provider-e2e_1.0.0_linux_amd64.zip"
	zip := []byte("PK\x05\x06" + strings.Repeat("\x00", 18))
	uploadFile(t, "local-terraform", path, zip, "application/zip")
	assertWriteGated(t, http.MethodPut, api("/api/v2/remotes/local-terraform/files/myorg/e2e/terraform-provider-e2e_1.1.0_linux_amd64.zip"), zip, "application/zip", api("/api/v2/remotes/local-terraform/files/"+path))
}

// TestLocalPyPIUpload uploads a wheel and validates the generated simple index.
func TestLocalPyPIUpload(t *testing.T) {
	createRepo(t, `{"name":"local-pypi","package_type":"pypi","repo_type":"local"}`)
	defer deleteRepo(t, "local-pypi")

	wheel := fixtureBytes(t, "packages/foo-1.0-py3-none-any.whl")
	uploadFile(t, "local-pypi", "foo-1.0-py3-none-any.whl", wheel, "application/zip")
	assertWriteGated(t, http.MethodPut, api("/api/v2/remotes/local-pypi/files/foo-2.0-py3-none-any.whl"), wheel, "application/zip", api("/api/v1/local/local-pypi/simple/foo/"))

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
	assertWriteGated(t, http.MethodPut, api("/api/v2/remotes/local-rpm/files/e2e-testpkg-1.0-1.noarch.rpm"), rpm, "application/x-rpm", api("/api/v1/local/local-rpm/repodata/repomd.xml"))
}

// TestLocalDebRepo uploads a .deb and validates that the flat apt index
// (Packages / Release) is generated automatically from the parsed control
// stanza (the deb-local analog of rpm repodata generation).
func TestLocalDebRepo(t *testing.T) {
	createRepo(t, `{"name":"local-deb","package_type":"deb","repo_type":"local"}`)
	defer deleteRepo(t, "local-deb")

	deb := testsupport.MinimalDeb("e2e-testpkg", "1.0.0", "amd64")
	uploadFile(t, "local-deb", "e2e-testpkg_1.0.0_amd64.deb", deb, "application/vnd.debian.binary-package")

	// The index is served (empty) at once and lists the package only once the
	// upload is parsed asynchronously; poll until it does.
	var pkgs string
	for deadline := time.Now().Add(15 * time.Second); !strings.Contains(pkgs, "Package: e2e-testpkg") && time.Now().Before(deadline); time.Sleep(250 * time.Millisecond) {
		resp, body := doRequest(t, http.MethodGet, api("/api/v1/local/local-deb/Packages"), nil, "")
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("Packages: status %d: %s", resp.StatusCode, body)
		}
		pkgs = string(body)
	}
	for _, want := range []string{"Package: e2e-testpkg", "Version: 1.0.0", "Architecture: amd64", "Filename: pool/e2e-testpkg_1.0.0_amd64.deb", "SHA256:"} {
		if !strings.Contains(pkgs, want) {
			t.Fatalf("Packages missing %q:\n%s", want, pkgs)
		}
	}

	resp, body := doRequest(t, http.MethodGet, api("/api/v1/local/local-deb/Release"), nil, "")
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
	assertWriteGated(t, http.MethodPut, api("/api/v2/remotes/local-deb/files/e2e-testpkg_2.0.0_amd64.deb"), deb, "application/vnd.debian.binary-package", api("/api/v1/local/local-deb/Packages"))
}

// TestLocalAlpineIndex uploads an .apk to an alpine local repo and validates
// that a per-arch APKINDEX.tar.gz is generated automatically from the parsed
// .PKGINFO (the apk-local analog of rpm repodata / deb Packages generation).
func TestLocalAlpineIndex(t *testing.T) {
	createRepo(t, `{"name":"local-alpine","package_type":"alpine","repo_type":"local"}`)
	defer deleteRepo(t, "local-alpine")

	apk := testsupport.MinimalApk("e2e-testpkg", "1.0-r0", "x86_64")
	uploadFile(t, "local-alpine", "x86_64/e2e-testpkg-1.0-r0.apk", apk, "application/vnd.android.package-archive")

	// The index is served (empty) at once but lists the package only once the
	// upload's metadata is parsed asynchronously; poll until it does.
	var index string
	for deadline := time.Now().Add(15 * time.Second); !strings.Contains(index, "P:e2e-testpkg") && time.Now().Before(deadline); time.Sleep(250 * time.Millisecond) {
		resp, body := doRequest(t, http.MethodGet, api("/api/v1/local/local-alpine/x86_64/APKINDEX.tar.gz"), nil, "")
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("APKINDEX: status %d: %s", resp.StatusCode, body)
		}
		index = apkIndexText(t, body)
	}
	for _, want := range []string{"P:e2e-testpkg", "V:1.0-r0", "A:x86_64", "C:Q1", "S:", "I:"} {
		if !strings.Contains(index, want) {
			t.Fatalf("APKINDEX missing %q:\n%s", want, index)
		}
	}

	// The .apk downloads back byte-identical from its arch path.
	resp, body := doRequest(t, http.MethodGet, api("/api/v1/local/local-alpine/x86_64/e2e-testpkg-1.0-r0.apk"), nil, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("download apk: status %d: %s", resp.StatusCode, body)
	}
	if !bytes.Equal(body, apk) {
		t.Fatalf("apk content mismatch")
	}
	assertWriteGated(t, http.MethodPut, api("/api/v2/remotes/local-alpine/files/x86_64/e2e-testpkg-2.0-r0.apk"), apk, "application/vnd.android.package-archive", api("/api/v1/local/local-alpine/x86_64/APKINDEX.tar.gz"))
}

func apkIndexText(t *testing.T, body []byte) string {
	t.Helper()
	zr, err := gzip.NewReader(bytes.NewReader(body))
	if err != nil {
		t.Fatalf("APKINDEX not gzip: %v", err)
	}
	tr := tar.NewReader(zr)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return ""
		}
		if err != nil {
			t.Fatalf("APKINDEX not tar: %v", err)
		}
		if hdr.Name == "APKINDEX" {
			b, _ := io.ReadAll(tr)
			return string(b)
		}
	}
}
