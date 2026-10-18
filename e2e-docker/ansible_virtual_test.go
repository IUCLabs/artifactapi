//go:build dockere2e

package e2edocker

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// TestVirtualAnsibleInstall publishes a collection to the second of two
// ansible locals and installs it with stock ansible-galaxy through a virtual
// over both, the download 302ing to the owning member.
func TestVirtualAnsibleInstall(t *testing.T) {
	network := os.Getenv("COMPOSE_NETWORK")
	internal := os.Getenv("ARTIFACTAPI_INTERNAL")
	if network == "" || internal == "" {
		t.Skip("COMPOSE_NETWORK/ARTIFACTAPI_INTERNAL not set; run via scripts/docker-e2e.sh")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not available on the test host")
	}

	id := time.Now().UnixNano()
	a, b, virt := fmt.Sprintf("e2e-ansible-a-%d", id), fmt.Sprintf("e2e-ansible-b-%d", id), fmt.Sprintf("e2e-ansible-v-%d", id)
	for _, name := range []string{a, b} {
		createRepo(t, fmt.Sprintf(`{"name":%q,"package_type":"ansible","repo_type":"local"}`, name))
		defer deleteRepo(t, name)
	}
	createVirtual(t, fmt.Sprintf(`{"name":%q,"package_type":"ansible","members":[%q,%q]}`, virt, a, b))
	defer deleteVirtual(t, virt)

	base := strings.TrimRight(internal, "/")
	script := "set -euo pipefail\n" + ansibleHelloScript + `
ansible-galaxy collection publish /w/dist/myorg-e2e_hello-1.0.0.tar.gz --server "$BASE/api/v1/local/$B/" --api-key unused
V="$BASE/api/v1/virtual/$VIRT"
ansible-galaxy collection install myorg.e2e_hello --server "$V/" -p /w/coll
DL=$(python3 -c 'import json,sys,urllib.request as u;print(json.load(u.urlopen(sys.argv[1]))["download_url"])' "$V/api/v3/collections/myorg/e2e_hello/versions/1.0.0/")
echo "REDIRECT $(python3 -c 'import sys,urllib.request as u
class N(u.HTTPRedirectHandler):
    def redirect_request(self,*a): return None
try: u.build_opener(N).open(sys.argv[1])
except u.HTTPError as e: print(e.code, e.headers["Location"])' "$DL")"
` + ansibleHelloPlay

	out, err := exec.Command("docker", "run", "--rm", "--network", network,
		"-e", "BASE="+base, "-e", "B="+b, "-e", "VIRT="+virt,
		"python:3.12-slim", "bash", "-c", script).CombinedOutput()
	if err != nil {
		t.Fatalf("ansible-galaxy client run failed: %v\n%s", err, out)
	}
	for _, want := range []string{
		"myorg.e2e_hello:1.0.0 was installed successfully",
		"REDIRECT 302 " + base + "/api/v1/local/" + b + "/artifacts/myorg-e2e_hello-1.0.0.tar.gz",
		"PLAY hello e2e",
	} {
		if !strings.Contains(string(out), want) {
			t.Fatalf("missing %q in client output:\n%s", want, out)
		}
	}
}
