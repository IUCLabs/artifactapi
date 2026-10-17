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

// TestVirtualGemsInstall pushes to two local gems repos and drives stock
// bundler through a virtual over both: merged versions, member order winning a
// duplicate version, and downloads redirected to the owning member.
func TestVirtualGemsInstall(t *testing.T) {
	network := os.Getenv("COMPOSE_NETWORK")
	internal := os.Getenv("ARTIFACTAPI_INTERNAL")
	if network == "" || internal == "" {
		t.Skip("COMPOSE_NETWORK/ARTIFACTAPI_INTERNAL not set; run via scripts/docker-e2e.sh")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not available on the test host")
	}

	id := time.Now().UnixNano()
	a, b, virt := fmt.Sprintf("e2e-gems-a-%d", id), fmt.Sprintf("e2e-gems-b-%d", id), fmt.Sprintf("e2e-gems-v-%d", id)
	for _, name := range []string{a, b} {
		createRepo(t, fmt.Sprintf(`{"name":%q,"package_type":"gems","repo_type":"local"}`, name))
		defer deleteRepo(t, name)
	}
	createVirtual(t, fmt.Sprintf(`{"name":%q,"package_type":"gems","members":[%q,%q]}`, virt, a, b))
	defer deleteVirtual(t, virt)

	base := strings.TrimRight(internal, "/")
	script := `set -euo pipefail
export GEM_HOST_API_KEY=unused
pub() { d=/w/src-$1-$2; mkdir -p $d/lib; cd $d
  echo "module MyorgE2eHello; VERSION = '$1:$2'; end" > lib/myorg-e2e-hello.rb
  printf 'Gem::Specification.new { |s| s.name = "myorg-e2e-hello"; s.version = "%s"; s.summary = "e2e"; s.authors = ["e2e"]; s.files = ["lib/myorg-e2e-hello.rb"] }' "$2" > g.gemspec
  gem build g.gemspec >/dev/null 2>&1; gem push --host "$BASE/api/v1/local/$1" myorg-e2e-hello-$2.gem; }
pub "$A" 0.1.0
pub "$B" 0.1.0
pub "$B" 0.2.0
V="$BASE/api/v1/virtual/$V"
echo "VERSIONS $(curl -s "$V/versions" | grep '^myorg-e2e-hello ' | cut -d' ' -f2)"
mkdir -p /w/app && cd /w/app
printf 'source "%s/"\ngem "myorg-e2e-hello", "0.1.0"\n' "$V" > Gemfile
bundle install
bundle exec ruby -e 'require "myorg-e2e-hello"; puts "REQ #{MyorgE2eHello::VERSION}"'
echo "REDIRECT $(curl -s -o /dev/null -w '%{redirect_url}' "$V/gems/myorg-e2e-hello-0.2.0.gem")"`

	out, err := exec.Command("docker", "run", "--rm", "--network", network,
		"-e", "BASE="+base, "-e", "A="+a, "-e", "B="+b, "-e", "V="+virt,
		"ruby:3.3", "bash", "-c", script).CombinedOutput()
	if err != nil {
		t.Fatalf("bundler run failed: %v\n%s", err, out)
	}
	for _, want := range []string{
		"VERSIONS 0.1.0,0.2.0",
		"REQ " + a + ":0.1.0",
		"REDIRECT " + base + "/api/v1/local/" + b + "/gems/myorg-e2e-hello-0.2.0.gem",
	} {
		if !strings.Contains(string(out), want) {
			t.Fatalf("missing %q in bundler output:\n%s", want, out)
		}
	}
}
