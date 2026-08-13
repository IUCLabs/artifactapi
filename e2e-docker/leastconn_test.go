//go:build dockere2e

package e2edocker

import (
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// TestLeastConnMultiBaseURL configures an rpm remote with mirror_strategy =
// least_conn over a two-mirror pool and drives distinct cache-miss paths through
// it, asserting every request succeeds and both mirrors serve traffic. This
// exercises the least-connections selection path (leastConnOrder + the in-flight
// gauge inc/dec around each upstream call) end-to-end through a real HTTP client.
// A precise least-loaded assertion is timing-sensitive over HTTP and is covered
// deterministically by the proxy unit test (TestLeastConnPicksLeastLoaded);
// here, with requests issued serially, in-flight counts return to zero between
// them so equal-load mirrors are spread by the round-robin tie-break.
func TestLeastConnMultiBaseURL(t *testing.T) {
	name := "e2e-leastconn"
	createRepo(t, fmt.Sprintf(`{
		"name": %q,
		"package_type": "rpm",
		"repo_type": "remote",
		"base_url": %q,
		"mirrorlist": [%q],
		"mirror_strategy": "least_conn",
		"stale_on_error": false
	}`, name, mockUpstreamA(), mockUpstreamB()))
	defer deleteRepo(t, name)

	seenA, seenB := false, false
	const n = 12
	for i := 0; i < n; i++ {
		url := api(fmt.Sprintf("/api/v1/remote/%s/lc/%d", name, i))
		resp, body := doRequest(t, http.MethodGet, url, nil, "")
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("request %d: status %d: %s", i, resp.StatusCode, body)
		}
		switch strings.TrimSpace(string(body)) {
		case "UPSTREAM-A":
			seenA = true
		case "UPSTREAM-B":
			seenB = true
		default:
			t.Fatalf("request %d: unexpected body %q", i, body)
		}
	}
	if !seenA || !seenB {
		t.Fatalf("least_conn remote did not reach both upstreams: A=%v B=%v", seenA, seenB)
	}
}

// TestLeastConnDnfInstall drives a real dnf (stock rockylinux container) at a
// two-mirror rpm remote configured with mirror_strategy = least_conn whose
// primary base_url is dead: makecache + install must succeed via the live mirror.
// This proves a real package-manager client installs correctly through a
// least_conn remote and that failover semantics are unchanged under the new
// strategy. Requires the compose network exported by scripts/docker-e2e.sh.
func TestLeastConnDnfInstall(t *testing.T) {
	network := os.Getenv("COMPOSE_NETWORK")
	internal := os.Getenv("ARTIFACTAPI_INTERNAL")
	if network == "" || internal == "" {
		t.Skip("COMPOSE_NETWORK/ARTIFACTAPI_INTERNAL not set; run via scripts/docker-e2e.sh")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not available on the test host")
	}

	name := "e2e-leastconn-dnf"
	createRepo(t, fmt.Sprintf(`{
		"name": %q,
		"package_type": "rpm",
		"repo_type": "remote",
		"base_url": "http://mockupstream-dead:80",
		"mirrorlist": [%q],
		"mirror_strategy": "least_conn",
		"stale_on_error": false
	}`, name, mockUpstream()))
	defer deleteRepo(t, name)

	repoURL := strings.TrimRight(internal, "/") + "/api/v1/remote/" + name + "/rpm-mirror"
	repoConf := fmt.Sprintf("[dnflc]\nname=dnflc\nbaseurl=%s\nenabled=1\ngpgcheck=0\nsslverify=0\nmetadata_expire=0\n", repoURL)
	script := "set -euo pipefail; " +
		"printf '%s' \"$REPO\" > /etc/yum.repos.d/dnflc.repo; " +
		"dnf -y --disablerepo='*' --enablerepo=dnflc makecache; " +
		"dnf -y --disablerepo='*' --enablerepo=dnflc install e2e-testpkg; " +
		"rpm -q e2e-testpkg"

	cmd := exec.Command("docker", "run", "--rm",
		"--network", network,
		"-e", "REPO="+repoConf,
		"rockylinux:9", "bash", "-c", script)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("real dnf install through a least_conn remote failed: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "e2e-testpkg-1.0-1") {
		t.Fatalf("dnf did not install the expected package via least_conn remote; output:\n%s", out)
	}
}
