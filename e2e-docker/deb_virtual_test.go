//go:build dockere2e

package e2edocker

import (
	"net/http"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/IUCLabs/artifactapi/internal/testsupport"
)

// TestVirtualDebMerge merges two local deb repos carrying the same package: the
// first member wins the duplicate, and a real apt sees both members' packages
// and installs through the virtual.
func TestVirtualDebMerge(t *testing.T) {
	createRepo(t, `{"name":"vdeb-a","package_type":"deb","repo_type":"local"}`)
	createRepo(t, `{"name":"vdeb-b","package_type":"deb","repo_type":"local"}`)
	defer deleteRepo(t, "vdeb-a")
	defer deleteRepo(t, "vdeb-b")

	uploadFile(t, "vdeb-a", "vdeb-one_1.0.0_amd64.deb", testsupport.MinimalDeb("vdeb-one", "1.0.0", "amd64"), "application/vnd.debian.binary-package")
	uploadFile(t, "vdeb-b", "vdeb-one_1.0.0_amd64.deb", testsupport.MinimalDeb("vdeb-one", "1.0.0", "amd64"), "application/vnd.debian.binary-package")
	uploadFile(t, "vdeb-b", "vdeb-two_2.0.0_amd64.deb", testsupport.MinimalDeb("vdeb-two", "2.0.0", "amd64"), "application/vnd.debian.binary-package")

	// Local indexes regenerate asynchronously; merge only once vdeb-b lists both.
	deadline := time.Now().Add(15 * time.Second)
	for {
		resp, body := doRequest(t, http.MethodGet, api("/api/v1/local/vdeb-b/Packages"), nil, "")
		if resp.StatusCode == http.StatusOK && strings.Contains(string(body), "vdeb-two") || time.Now().After(deadline) {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}

	createVirtual(t, `{"name":"vdeb","package_type":"deb","members":["vdeb-a","vdeb-b"]}`)
	defer deleteVirtual(t, "vdeb")

	_, body := doRequest(t, http.MethodGet, api("/api/v1/virtual/vdeb/Packages"), nil, "")
	pk := string(body)
	if strings.Count(pk, "Package: vdeb-one\n") != 1 || !strings.Contains(pk, "Filename: vdeb-a/pool/vdeb-one_1.0.0_amd64.deb") ||
		!strings.Contains(pk, "Filename: vdeb-b/pool/vdeb-two_2.0.0_amd64.deb") {
		t.Fatalf("merged Packages should hold vdeb-one from the first member and vdeb-two:\n%s", pk)
	}

	network := os.Getenv("COMPOSE_NETWORK")
	internal := os.Getenv("ARTIFACTAPI_INTERNAL")
	if network == "" || internal == "" {
		t.Skip("COMPOSE_NETWORK/ARTIFACTAPI_INTERNAL not set; skipping real apt")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not available on the test host")
	}
	script := "set -euo pipefail; " +
		"rm -f /etc/apt/sources.list.d/*; " +
		"echo \"deb [trusted=yes] $URL ./\" > /etc/apt/sources.list.d/vdeb.list; " +
		"apt-get update; " +
		"apt-cache policy vdeb-one vdeb-two | tee /dev/stderr | grep -c virtual/vdeb | grep -qx 2; " +
		"apt-get install -y vdeb-one vdeb-two; " +
		"dpkg -s vdeb-one vdeb-two"
	out, err := exec.Command("docker", "run", "--rm", "--network", network,
		"-e", "URL="+strings.TrimRight(internal, "/")+"/api/v1/virtual/vdeb/",
		"debian:13", "bash", "-c", script).CombinedOutput()
	if err != nil {
		t.Fatalf("real apt against deb virtual failed: %v\n%s", err, out)
	}
}

// TestVirtualDebSuite merges two proxied deb.debian.org remotes and drives a
// real apt through the virtual's dists/<suite> layout (by-hash indexes).
func TestVirtualDebSuite(t *testing.T) {
	network := os.Getenv("COMPOSE_NETWORK")
	internal := os.Getenv("ARTIFACTAPI_INTERNAL")
	if network == "" || internal == "" {
		t.Skip("COMPOSE_NETWORK/ARTIFACTAPI_INTERNAL not set; skipping real apt")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not available on the test host")
	}
	for _, n := range []string{"vdebs-a", "vdebs-b"} {
		createRepo(t, `{"name":"`+n+`","package_type":"deb","repo_type":"remote","base_url":"http://deb.debian.org/debian"}`)
		defer deleteRepo(t, n)
	}
	createVirtual(t, `{"name":"vdebs","package_type":"deb","members":["vdebs-a","vdebs-b"]}`)
	defer deleteVirtual(t, "vdebs")

	resp, rel := doRequest(t, http.MethodGet, api("/api/v1/virtual/vdebs/dists/trixie-updates/Release"), nil, "")
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(rel), "Acquire-By-Hash: yes\n") ||
		!strings.Contains(string(rel), " main/binary-amd64/Packages.gz\n") {
		t.Fatalf("merged suite Release: status %d:\n%s", resp.StatusCode, rel)
	}

	script := "set -euo pipefail; " +
		"rm -f /etc/apt/sources.list.d/*; " +
		"echo \"deb [trusted=yes] $URL trixie-updates main\" > /etc/apt/sources.list.d/vdebs.list; " +
		"apt-get update; " +
		"idx=/tmp/avail; apt-cache dumpavail > $idx; " +
		"grep -q '^Filename: vdebs-a/pool/' $idx; if grep -q '^Filename: vdebs-b/' $idx; then echo 'vdebs-b not deduped'; exit 1; fi; " +
		"pkg=$(awk '/^Package:/{p=$2} /^Size:/{if(!m||$2<m){m=$2;b=p}} END{print b}' $idx); " +
		"cd /tmp && apt-get download $pkg && ls -l /tmp/*.deb"
	out, err := exec.Command("docker", "run", "--rm", "--network", network,
		"-e", "URL="+strings.TrimRight(internal, "/")+"/api/v1/virtual/vdebs/",
		"debian:13", "bash", "-c", script).CombinedOutput()
	if err != nil {
		t.Fatalf("real apt against deb virtual suite failed: %v\n%s", err, out)
	}
	t.Logf("%s", out)
	resp, _ = doRequest(t, http.MethodGet, api("/api/v1/remote/vdebs-a/dists/trixie-updates/main/binary-amd64/Packages.xz"), nil, "")
	if src := resp.Header.Get("X-Artifact-Source"); resp.StatusCode != http.StatusOK || src != "cache" {
		t.Fatalf("member index the virtual merged should be cached: status %d source %q", resp.StatusCode, src)
	}
}
