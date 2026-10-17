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

// TestLocalNPMPublishInstall drives a stock npm client against a local npm
// repo: publish unscoped and scoped packages, install and require() them in a
// fresh project, publish a second version, and reject a republish.
func TestLocalNPMPublishInstall(t *testing.T) {
	network := os.Getenv("COMPOSE_NETWORK")
	internal := os.Getenv("ARTIFACTAPI_INTERNAL")
	if network == "" || internal == "" {
		t.Skip("COMPOSE_NETWORK/ARTIFACTAPI_INTERNAL not set; run via scripts/docker-e2e.sh")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not available on the test host")
	}

	name := fmt.Sprintf("e2e-npm-local-%d", time.Now().UnixNano())
	createRepo(t, fmt.Sprintf(`{"name":%q,"package_type":"npm","repo_type":"local"}`, name))
	defer deleteRepo(t, name)

	reg := strings.TrimRight(internal, "/") + "/api/v1/local/" + name + "/"
	script := `set -euo pipefail
printf 'registry=%s\n%s:_authToken=unused\n' "$REG" "${REG#http:}" > ~/.npmrc
mk() { d="/w/src/$1-$2"; mkdir -p "$d"; cd "$d"
  printf '{"name":"%s","version":"%s","main":"index.js"}' "$1" "$2" > package.json
  echo "module.exports = '$1@$2';" > index.js; }
mk e2e-npm-plain 1.0.0 && npm publish
mk @e2e/npm-scoped 1.0.0 && npm publish
mk e2e-npm-plain 1.0.0
if npm publish 2>/dev/null; then echo "republish accepted"; exit 1; fi
mkdir -p /w/app && cd /w/app && npm init -y >/dev/null
npm install --no-audit --cache /tmp/fresh-cache e2e-npm-plain @e2e/npm-scoped
node -e "console.log('REQ', require('e2e-npm-plain'), require('@e2e/npm-scoped'))"
mk e2e-npm-plain 1.1.0 && npm publish
echo "VERSIONS $(npm view e2e-npm-plain versions --json | tr -d ' \n')"
echo "LATEST $(npm view e2e-npm-plain dist-tags.latest)"`

	out, err := exec.Command("docker", "run", "--rm", "--network", network,
		"-e", "REG="+reg, "node:22", "bash", "-c", script).CombinedOutput()
	if err != nil {
		t.Fatalf("npm client run failed: %v\n%s", err, out)
	}
	for _, want := range []string{
		"REQ e2e-npm-plain@1.0.0 @e2e/npm-scoped@1.0.0",
		`VERSIONS ["1.0.0","1.1.0"]`,
		"LATEST 1.1.0",
	} {
		if !strings.Contains(string(out), want) {
			t.Fatalf("missing %q in npm output:\n%s", want, out)
		}
	}
}
