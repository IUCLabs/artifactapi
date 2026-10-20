//go:build dockere2e

package e2edocker

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

const puppetHelloMetadata = `{"name":"myorg-hello","version":"0.1.0","author":"myorg","summary":"e2e","license":"Apache-2.0","source":"https://example.invalid","dependencies":[{"name":"puppetlabs/stdlib","version_requirement":">= 4.0.0 < 10.0.0"}]}`

func puppetModuleTarball(t *testing.T) []byte {
	t.Helper()
	return puppetTarball(t, "myorg-hello-0.1.0", puppetHelloMetadata)
}

func puppetTarball(t *testing.T, dir, metadata string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	for name, body := range map[string]string{
		dir + "/metadata.json":     metadata,
		dir + "/manifests/init.pp": "class stub {}\n",
	} {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(body)), ModTime: time.Now()}); err != nil {
			t.Fatal(err)
		}
		_, _ = tw.Write([]byte(body))
	}
	_ = tw.Close()
	_ = zw.Close()
	return buf.Bytes()
}

// TestLocalPuppetRealClient publishes a module to a puppet local and installs
// it with the real r10k and puppet module tool. The stdlib dependency is not on
// a local-only forge, so puppet runs with --ignore-dependencies.
func TestLocalPuppetRealClient(t *testing.T) {
	network := os.Getenv("COMPOSE_NETWORK")
	internal := strings.TrimRight(os.Getenv("ARTIFACTAPI_INTERNAL"), "/")
	if network == "" || internal == "" {
		t.Skip("COMPOSE_NETWORK/ARTIFACTAPI_INTERNAL not set; run via scripts/docker-e2e.sh")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not available on the test host")
	}

	createRepo(t, `{"name":"local-puppet","package_type":"puppet","repo_type":"local"}`)
	defer deleteRepo(t, "local-puppet")

	upload := api("/api/v2/remotes/local-puppet/files/myorg-hello-0.1.0.tar.gz")
	defer doRequest(t, http.MethodDelete, api("/api/v2/remotes/local-puppet/files/v3/files/myorg-hello-0.1.0.tar.gz"), nil, "")
	tarball := puppetModuleTarball(t)
	if resp, body := doRequest(t, http.MethodPut, upload, tarball, "application/gzip"); resp.StatusCode != http.StatusCreated {
		t.Fatalf("upload: %d %s", resp.StatusCode, body)
	}
	if resp, _ := doRequest(t, http.MethodPut, upload, tarball, "application/gzip"); resp.StatusCode != http.StatusConflict {
		t.Fatalf("re-upload of an existing version: %d, want 409", resp.StatusCode)
	}
	if resp, body := getEventually(t, api("/api/v1/local/local-puppet/v3/releases/myorg-hello-0.1.0"), 15*time.Second); resp.StatusCode != http.StatusOK || !strings.Contains(string(body), `"file_md5"`) {
		t.Fatalf("release never indexed: %d %s", resp.StatusCode, body)
	}

	forge := internal + "/api/v1/local/local-puppet"
	run := func(image, script string) string {
		t.Helper()
		out, err := exec.Command("docker", "run", "--rm", "--network", network, "--entrypoint", "sh", image, "-c", "set -eu; "+script).CombinedOutput()
		if err != nil {
			t.Fatalf("%s failed: %v\n%s", image, err, out)
		}
		return string(out)
	}

	out := run("ghcr.io/voxpupuli/r10k:latest", "cd /tmp && printf 'forge:\\n  baseurl: "+forge+"\\n' > r10k.yaml && "+
		"printf \"forge '"+forge+"'\\nmod 'myorg-hello', '0.1.0'\\n\" > Puppetfile && "+
		"r10k puppetfile install -c r10k.yaml --verbose && cat modules/hello/metadata.json")
	if !strings.Contains(out, `"myorg-hello"`) {
		t.Fatalf("r10k did not install myorg-hello:\n%s", out)
	}

	out = run("ghcr.io/openvoxproject/openvoxagent:latest", "export PATH=/opt/puppetlabs/bin:$PATH; puppet module install myorg-hello --module_repository "+forge+" --ignore-dependencies --target-dir /tmp/mods && puppet module list --color=false --modulepath /tmp/mods")
	if !strings.Contains(out, "myorg-hello (v0.1.0)") {
		t.Fatalf("puppet module install did not install myorg-hello:\n%s", out)
	}
}
