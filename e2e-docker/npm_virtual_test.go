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

// TestVirtualNPMInstall publishes to two local npm repos and drives a stock
// npm client through a virtual over both: merged versions and dist-tags, and
// an install whose tarballs resolve via the virtual's member redirect.
func TestVirtualNPMInstall(t *testing.T) {
	network := os.Getenv("COMPOSE_NETWORK")
	internal := os.Getenv("ARTIFACTAPI_INTERNAL")
	if network == "" || internal == "" {
		t.Skip("COMPOSE_NETWORK/ARTIFACTAPI_INTERNAL not set; run via scripts/docker-e2e.sh")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not available on the test host")
	}

	id := time.Now().UnixNano()
	a, b, virt := fmt.Sprintf("e2e-npm-a-%d", id), fmt.Sprintf("e2e-npm-b-%d", id), fmt.Sprintf("e2e-npm-v-%d", id)
	for _, name := range []string{a, b} {
		createRepo(t, fmt.Sprintf(`{"name":%q,"package_type":"npm","repo_type":"local"}`, name))
		defer deleteRepo(t, name)
	}
	createVirtual(t, fmt.Sprintf(`{"name":%q,"package_type":"npm","members":[%q,%q]}`, virt, a, b))
	defer deleteVirtual(t, virt)

	base := strings.TrimRight(internal, "/")
	script := `set -euo pipefail
pub() { d="/w/src/$2-$3-$1"; mkdir -p "$d"; cd "$d"
  printf '{"name":"%s","version":"%s","main":"index.js"}' "$2" "$3" > package.json
  echo "module.exports = '$1:$2@$3';" > index.js
  printf 'registry=%s\n%s:_authToken=%s\n' "$BASE/api/v1/local/$1/" "${BASE#http:}/api/v1/local/$1/" "$TOKEN" > .npmrc
  npm publish; }
pub "$A" @e2e/virt 1.0.0
pub "$B" @e2e/virt 1.0.0
pub "$B" @e2e/virt 1.1.0
mkdir -p /w/app && cd /w/app && npm init -y >/dev/null
echo "registry=$BASE/api/v1/virtual/$V/" > .npmrc
echo "VERSIONS $(npm view @e2e/virt versions --json | tr -d ' \n')"
npm install --no-audit --cache /tmp/fresh-cache @e2e/virt@1.0.0
node -e "console.log('REQ', require('@e2e/virt'))"
echo "RESOLVED $(node -p "require('./package-lock.json').packages['node_modules/@e2e/virt'].resolved")"`

	out, err := exec.Command("docker", "run", "--rm", "--network", network,
		"-e", "BASE="+base, "-e", "A="+a, "-e", "B="+b, "-e", "V="+virt, "-e", "TOKEN="+authToken,
		"node:22", "bash", "-c", script).CombinedOutput()
	if err != nil {
		t.Fatalf("npm client run failed: %v\n%s", err, out)
	}
	for _, want := range []string{
		`VERSIONS ["1.0.0","1.1.0"]`,
		"REQ " + a + ":@e2e/virt@1.0.0",
		"RESOLVED " + base + "/api/v1/virtual/" + virt + "/" + a + "/@e2e/virt/-/virt-1.0.0.tgz",
	} {
		if !strings.Contains(string(out), want) {
			t.Fatalf("missing %q in npm output:\n%s", want, out)
		}
	}
}
