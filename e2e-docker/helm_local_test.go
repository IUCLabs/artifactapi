//go:build dockere2e

package e2edocker

import (
	"net/http"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// TestLocalHelmRealClient packages a chart with the real helm CLI, publishes it
// to a helm local, then drives helm repo add/search/pull/template against the
// local and against a helm virtual that has the local as a member.
func TestLocalHelmRealClient(t *testing.T) {
	network := os.Getenv("COMPOSE_NETWORK")
	internal := strings.TrimRight(os.Getenv("ARTIFACTAPI_INTERNAL"), "/")
	if network == "" || internal == "" {
		t.Skip("COMPOSE_NETWORK/ARTIFACTAPI_INTERNAL not set; run via scripts/docker-e2e.sh")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not available on the test host")
	}

	createRepo(t, `{"name":"local-helm","package_type":"helm","repo_type":"local"}`)
	defer deleteRepo(t, "local-helm")
	createVirtual(t, `{"name":"vhelm","package_type":"helm","members":["local-helm"]}`)
	defer deleteVirtual(t, "vhelm")

	helm := func(t *testing.T, script string) string {
		t.Helper()
		out, err := exec.Command("docker", "run", "--rm", "--network", network, "-e", "TOKEN="+authToken, "-e", "BAD="+badToken,
			"--entrypoint", "sh", "alpine/helm:3.16.4", "-c", "set -eu; "+script).Output()
		if err != nil {
			stderr := ""
			if ee, ok := err.(*exec.ExitError); ok {
				stderr = string(ee.Stderr)
			}
			t.Fatalf("helm script failed: %v\n%s%s", err, out, stderr)
		}
		return string(out)
	}

	chart := helm(t, "cd /tmp && helm create e2echart >/dev/null && helm package e2echart >/dev/null && cat e2echart-0.1.0.tgz")
	uploadFile(t, "local-helm", "e2echart-0.1.0.tgz", []byte(chart), "application/gzip")

	// Chart metadata is recorded asynchronously after upload; poll the index.
	var body []byte
	for deadline := time.Now().Add(15 * time.Second); time.Now().Before(deadline); time.Sleep(500 * time.Millisecond) {
		var resp *http.Response
		resp, body = doRequest(t, http.MethodGet, api("/api/v1/local/local-helm/index.yaml"), nil, "")
		if resp.StatusCode == http.StatusOK && strings.Contains(string(body), "e2echart-0.1.0.tgz") {
			break
		}
	}
	if !strings.Contains(string(body), "e2echart-0.1.0.tgz") || !strings.Contains(string(body), "digest: ") {
		t.Fatalf("index.yaml never listed the chart:\n%s", body)
	}
	assertWriteGated(t, http.MethodPut, api("/api/v2/remotes/local-helm/files/e2echart-0.2.0.tgz"), []byte(chart), "application/gzip", api("/api/v1/local/local-helm/index.yaml"))

	for _, repoURL := range []string{internal + "/api/v1/local/local-helm", internal + "/api/v1/virtual/vhelm"} {
		out := helm(t, "helm repo add e2e "+repoURL+" >/dev/null && "+
			"helm search repo e2e/e2echart && "+
			"cd /tmp && helm pull e2e/e2echart --version 0.1.0 && "+
			"helm template rel e2echart-0.1.0.tgz | grep -c 'kind: Deployment'")
		if !strings.Contains(out, "e2e/e2echart") || !strings.Contains(out, "0.1.0") || !strings.HasSuffix(strings.TrimSpace(out), "1") {
			t.Fatalf("helm via %s:\n%s", repoURL, out)
		}
	}

	t.Run("oci", func(t *testing.T) {
		t.Skip("helm (oras-go) fetches the registry token with an OAuth2 POST form the server ignores; fixed by myorg/artifactapi#180")
		createRepo(t, `{"name":"local-helm-oci","package_type":"docker","repo_type":"local"}`)
		defer deleteRepo(t, "local-helm-oci")
		reg := strings.TrimPrefix(internal, "http://")
		out := helm(t, "cd /tmp && helm create e2echart >/dev/null && helm package e2echart >/dev/null && "+
			"echo \"$BAD\" | helm registry login "+reg+" -u ci --password-stdin --insecure && "+
			"if helm push e2echart-0.1.0.tgz oci://"+reg+"/local-helm-oci --plain-http; then echo 'invalid token push accepted'; exit 1; fi && "+
			"echo \"$TOKEN\" | helm registry login "+reg+" -u ci --password-stdin --insecure && "+
			"helm push e2echart-0.1.0.tgz oci://"+reg+"/local-helm-oci --plain-http && "+
			"helm registry logout "+reg+" && rm -rf e2echart* && "+
			"helm pull oci://"+reg+"/local-helm-oci/e2echart --version 0.1.0 --plain-http && ls e2echart-0.1.0.tgz")
		if !strings.Contains(out, "e2echart-0.1.0.tgz") {
			t.Fatalf("helm OCI push/pull:\n%s", out)
		}
	})
}
