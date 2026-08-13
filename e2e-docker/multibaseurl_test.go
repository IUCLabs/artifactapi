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

// mockUpstreamA/B are the constant-body upstreams (see docker-compose.e2e.yml)
// that let the round-robin test observe which mirror served each request.
func mockUpstreamA() string {
	if v := os.Getenv("MOCK_UPSTREAM_A_INTERNAL"); v != "" {
		return strings.TrimRight(v, "/")
	}
	return "http://mockupstreama"
}

func mockUpstreamB() string {
	if v := os.Getenv("MOCK_UPSTREAM_B_INTERNAL"); v != "" {
		return strings.TrimRight(v, "/")
	}
	return "http://mockupstreamb"
}

// TestMultiBaseURLRoundRobin configures an rpm remote with base_url = mirror A
// and mirrorlist = [mirror B] and drives distinct paths through it, asserting
// both mirrors serve traffic. Each path is a cache miss, so every request reaches
// upstream and the round-robin cursor alternates mirrors.
func TestMultiBaseURLRoundRobin(t *testing.T) {
	name := "e2e-rr"
	createRepo(t, fmt.Sprintf(`{
		"name": %q,
		"package_type": "rpm",
		"repo_type": "remote",
		"base_url": %q,
		"mirrorlist": [%q],
		"stale_on_error": false
	}`, name, mockUpstreamA(), mockUpstreamB()))
	defer deleteRepo(t, name)

	seenA, seenB := false, false
	const n = 12
	for i := 0; i < n; i++ {
		url := api(fmt.Sprintf("/api/v1/remote/%s/rr/%d", name, i))
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
		t.Fatalf("round-robin did not reach both upstreams: A=%v B=%v", seenA, seenB)
	}
}

// TestMultiBaseURLFailover points a two-mirror remote at a dead primary and a
// healthy secondary and asserts every request still succeeds via the secondary.
func TestMultiBaseURLFailover(t *testing.T) {
	name := "e2e-failover"
	createRepo(t, fmt.Sprintf(`{
		"name": %q,
		"package_type": "rpm",
		"repo_type": "remote",
		"base_url": "http://mockupstream-dead:80",
		"mirrorlist": [%q],
		"stale_on_error": false
	}`, name, mockUpstreamB()))
	defer deleteRepo(t, name)

	for i := 0; i < 6; i++ {
		url := api(fmt.Sprintf("/api/v1/remote/%s/fo/%d", name, i))
		resp, body := doRequest(t, http.MethodGet, url, nil, "")
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("request %d: dead primary broke fetch: status %d: %s", i, resp.StatusCode, body)
		}
		if got := strings.TrimSpace(string(body)); got != "UPSTREAM-B" {
			t.Fatalf("request %d: body %q, want UPSTREAM-B (served via failover)", i, got)
		}
	}
}

// TestSingleBaseURLRegression asserts a remote with no mirrorlist works exactly
// as before the mirrorlist change.
func TestSingleBaseURLRegression(t *testing.T) {
	name := "e2e-single"
	createRepo(t, fmt.Sprintf(`{
		"name": %q,
		"package_type": "rpm",
		"repo_type": "remote",
		"base_url": %q,
		"stale_on_error": false
	}`, name, mockUpstreamA()))
	defer deleteRepo(t, name)

	resp, body := doRequest(t, http.MethodGet, api("/api/v1/remote/"+name+"/solo/0"), nil, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("single-url fetch: status %d: %s", resp.StatusCode, body)
	}
	if got := strings.TrimSpace(string(body)); got != "UPSTREAM-A" {
		t.Fatalf("single-url body %q, want UPSTREAM-A", got)
	}
}

// TestMultiBaseURLDnfFailover drives a real dnf (stock rockylinux container) at
// a two-mirror rpm remote whose primary is dead: makecache + install must
// succeed via the live secondary mirror, proving a dead mirror does not break a
// real package-manager client. Requires the compose network and internal API
// URL exported by scripts/docker-e2e.sh; skipped when run standalone.
func TestMultiBaseURLDnfFailover(t *testing.T) {
	network := os.Getenv("COMPOSE_NETWORK")
	internal := os.Getenv("ARTIFACTAPI_INTERNAL")
	if network == "" || internal == "" {
		t.Skip("COMPOSE_NETWORK/ARTIFACTAPI_INTERNAL not set; run via scripts/docker-e2e.sh")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not available on the test host")
	}

	name := "e2e-dnf-failover"
	// Primary base_url is dead; the live mirror serves the real yum repo under
	// fixtures/rpm-mirror via the shared mock upstream.
	createRepo(t, fmt.Sprintf(`{
		"name": %q,
		"package_type": "rpm",
		"repo_type": "remote",
		"base_url": "http://mockupstream-dead:80",
		"mirrorlist": [%q],
		"stale_on_error": false
	}`, name, mockUpstream()))
	defer deleteRepo(t, name)

	repoURL := strings.TrimRight(internal, "/") + "/api/v1/remote/" + name + "/rpm-mirror"
	repoConf := fmt.Sprintf("[dnffo]\nname=dnffo\nbaseurl=%s\nenabled=1\ngpgcheck=0\nsslverify=0\nmetadata_expire=0\n", repoURL)
	script := "set -euo pipefail; " +
		"printf '%s' \"$REPO\" > /etc/yum.repos.d/dnffo.repo; " +
		"dnf -y --disablerepo='*' --enablerepo=dnffo makecache; " +
		"dnf -y --disablerepo='*' --enablerepo=dnffo install e2e-testpkg; " +
		"rpm -q e2e-testpkg"

	cmd := exec.Command("docker", "run", "--rm",
		"--network", network,
		"-e", "REPO="+repoConf,
		"rockylinux:9", "bash", "-c", script)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("real dnf install through a dead primary mirror failed: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "e2e-testpkg-1.0-1") {
		t.Fatalf("dnf did not install the expected package via failover; output:\n%s", out)
	}
}
