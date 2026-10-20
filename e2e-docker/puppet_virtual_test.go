//go:build dockere2e

package e2edocker

import (
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// TestVirtualPuppetInstall puts myorg-hello and its stdlib dependency on two
// puppet locals and installs through a virtual over both with the real puppet
// module tool (dependencies resolved) and r10k, downloads 302ing to each owner.
func TestVirtualPuppetInstall(t *testing.T) {
	network := os.Getenv("COMPOSE_NETWORK")
	internal := strings.TrimRight(os.Getenv("ARTIFACTAPI_INTERNAL"), "/")
	if network == "" || internal == "" {
		t.Skip("COMPOSE_NETWORK/ARTIFACTAPI_INTERNAL not set; run via scripts/docker-e2e.sh")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not available on the test host")
	}

	id := time.Now().UnixNano()
	a, b, virt := fmt.Sprintf("e2e-puppet-a-%d", id), fmt.Sprintf("e2e-puppet-b-%d", id), fmt.Sprintf("e2e-puppet-v-%d", id)
	modules := map[string]struct{ file, dir, meta string }{
		a: {"myorg-hello-0.1.0.tar.gz", "myorg-hello-0.1.0", puppetHelloMetadata},
		b: {"puppetlabs-stdlib-9.6.0.tar.gz", "puppetlabs-stdlib-9.6.0", `{"name":"puppetlabs-stdlib","version":"9.6.0","author":"puppetlabs","summary":"stub","license":"Apache-2.0","source":"https://example.invalid","dependencies":[]}`},
	}
	for repo, m := range modules {
		createRepo(t, fmt.Sprintf(`{"name":%q,"package_type":"puppet","repo_type":"local"}`, repo))
		defer deleteRepo(t, repo)
		if resp, body := doRequest(t, http.MethodPut, api("/api/v2/remotes/"+repo+"/files/"+m.file), puppetTarball(t, m.dir, m.meta), "application/gzip"); resp.StatusCode != http.StatusCreated {
			t.Fatalf("upload %s: %d %s", m.file, resp.StatusCode, body)
		}
		defer doRequest(t, http.MethodDelete, api("/api/v2/remotes/"+repo+"/files/v3/files/"+m.file), nil, "")
		if resp, body := getEventually(t, api("/api/v1/local/"+repo+"/v3/releases/"+strings.TrimSuffix(m.file, ".tar.gz")), 15*time.Second); resp.StatusCode != http.StatusOK {
			t.Fatalf("%s never indexed: %d %s", m.file, resp.StatusCode, body)
		}
	}
	createVirtual(t, fmt.Sprintf(`{"name":%q,"package_type":"puppet","members":[%q,%q]}`, virt, a, b))
	defer deleteVirtual(t, virt)

	forge := internal + "/api/v1/virtual/" + virt
	run := func(image, script string) string {
		t.Helper()
		out, err := exec.Command("docker", "run", "--rm", "--network", network, "--entrypoint", "sh", image, "-c", "set -eu; "+script).CombinedOutput()
		if err != nil {
			t.Fatalf("%s failed: %v\n%s", image, err, out)
		}
		return string(out)
	}

	out := run("ghcr.io/openvoxproject/openvoxagent:latest", "export PATH=/opt/puppetlabs/bin:$PATH; puppet module install myorg-hello --module_repository "+forge+" --target-dir /tmp/mods && puppet module list --color=false --modulepath /tmp/mods")
	for _, want := range []string{"myorg-hello (v0.1.0)", "puppetlabs-stdlib (v9.6.0)"} {
		if !strings.Contains(out, want) {
			t.Fatalf("puppet module install missing %q:\n%s", want, out)
		}
	}

	out = run("ghcr.io/voxpupuli/r10k:latest", "cd /tmp && printf 'forge:\\n  baseurl: "+forge+"\\n' > r10k.yaml && "+
		"printf \"mod 'myorg-hello', '0.1.0'\\nmod 'puppetlabs-stdlib', '9.6.0'\\n\" > Puppetfile && "+
		"r10k puppetfile install -c r10k.yaml && cat modules/hello/metadata.json modules/stdlib/metadata.json")
	if !strings.Contains(out, `"myorg-hello"`) || !strings.Contains(out, `"puppetlabs-stdlib"`) {
		t.Fatalf("r10k did not install both modules:\n%s", out)
	}

	noRedirect := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	for repo, m := range modules {
		resp, err := noRedirect.Get(api("/api/v1/virtual/" + virt + "/v3/files/" + m.file))
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if loc := resp.Header.Get("Location"); resp.StatusCode != http.StatusFound || !strings.HasSuffix(loc, "/api/v1/local/"+repo+"/v3/files/"+m.file) {
			t.Errorf("%s = %d %q, want 302 to %s", m.file, resp.StatusCode, loc, repo)
		}
	}
}
