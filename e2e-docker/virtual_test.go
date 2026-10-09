//go:build dockere2e

package e2edocker

import (
	"bytes"
	"compress/gzip"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
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

// TestVirtualRPMMerge merges a remote rpm repo and a local rpm repo carrying the
// same package: the first member wins the duplicate, its package download
// routes through the virtual, and a real dnf can consume the merged repo.
func TestVirtualRPMMerge(t *testing.T) {
	createRepo(t, `{"name":"vrpm-remote","package_type":"rpm","repo_type":"remote","base_url":"`+mockUpstream()+`/rpm-mirror","stale_on_error":true}`)
	createRepo(t, `{"name":"vrpm-local","package_type":"rpm","repo_type":"local"}`)
	defer deleteRepo(t, "vrpm-remote")
	defer deleteRepo(t, "vrpm-local")

	pkg := fixtureBytes(t, "rpmrepo/Packages/e2e-testpkg-1.0-1.noarch.rpm")
	uploadFile(t, "vrpm-local", "e2e-testpkg-1.0-1.noarch.rpm", pkg, "application/x-rpm")

	createVirtual(t, `{"name":"vrpm","package_type":"rpm","members":["vrpm-remote","vrpm-local"]}`)
	defer deleteVirtual(t, "vrpm")

	resp, body := getEventually(t, api("/api/v1/virtual/vrpm/repodata/repomd.xml"), 15*time.Second)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("virtual repomd.xml: status %d: %s", resp.StatusCode, body)
	}
	var md struct {
		Data []struct {
			Type     string `xml:"type,attr"`
			Location struct {
				Href string `xml:"href,attr"`
			} `xml:"location"`
		} `xml:"data"`
	}
	if err := xml.Unmarshal(body, &md); err != nil {
		t.Fatalf("parse repomd.xml: %v\n%s", err, body)
	}
	var primary []byte
	for _, d := range md.Data {
		if d.Type == "primary" {
			resp, gz := doRequest(t, http.MethodGet, api("/api/v1/virtual/vrpm/"+d.Location.Href), nil, "")
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("primary: status %d", resp.StatusCode)
			}
			r, err := gzip.NewReader(bytes.NewReader(gz))
			if err != nil {
				t.Fatal(err)
			}
			primary, _ = io.ReadAll(r)
		}
	}
	href := "vrpm-remote/Packages/e2e-testpkg-1.0-1.noarch.rpm"
	if strings.Count(string(primary), "<name>e2e-testpkg</name>") != 1 || !strings.Contains(string(primary), `href="`+href+`"`) {
		t.Fatalf("merged primary should hold one e2e-testpkg owned by the first member:\n%s", primary)
	}

	resp, got := doRequest(t, http.MethodGet, api("/api/v1/virtual/vrpm/"+href), nil, "")
	if resp.StatusCode != http.StatusOK || !bytes.Equal(got, pkg) {
		t.Fatalf("package via virtual: status %d, %d bytes (want %d)", resp.StatusCode, len(got), len(pkg))
	}

	network := os.Getenv("COMPOSE_NETWORK")
	internal := os.Getenv("ARTIFACTAPI_INTERNAL")
	if network == "" || internal == "" {
		t.Skip("COMPOSE_NETWORK/ARTIFACTAPI_INTERNAL not set; skipping real dnf")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not available on the test host")
	}
	repoConf := fmt.Sprintf("[vrpm]\nname=vrpm\nbaseurl=%s/api/v1/virtual/vrpm/\nenabled=1\ngpgcheck=0\nmetadata_expire=0\n", strings.TrimRight(internal, "/"))
	script := "set -euo pipefail; " +
		"printf '%s' \"$REPO\" > /etc/yum.repos.d/vrpm.repo; " +
		"dnf -y --disablerepo='*' --enablerepo=vrpm makecache; " +
		"dnf -y --disablerepo='*' --enablerepo=vrpm list --available; " +
		"dnf -y --disablerepo='*' --enablerepo=vrpm install e2e-testpkg; " +
		"rpm -q e2e-testpkg"
	out, err := exec.Command("docker", "run", "--rm", "--network", network, "-e", "REPO="+repoConf,
		"rockylinux:9", "bash", "-c", script).CombinedOutput()
	if err != nil {
		t.Fatalf("real dnf against rpm virtual failed: %v\n%s", err, out)
	}
}
