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

// TestLocalGemsPushInstall drives stock gem and bundler clients against a
// local gems repo: push two versions, reject a republish, gem install and
// bundle install the latest via the compact index, then yank it and check a
// re-push of the yanked version with different bytes is rejected.
func TestLocalGemsPushInstall(t *testing.T) {
	network := os.Getenv("COMPOSE_NETWORK")
	internal := os.Getenv("ARTIFACTAPI_INTERNAL")
	if network == "" || internal == "" {
		t.Skip("COMPOSE_NETWORK/ARTIFACTAPI_INTERNAL not set; run via scripts/docker-e2e.sh")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not available on the test host")
	}

	name := fmt.Sprintf("e2e-gems-local-%d", time.Now().UnixNano())
	createRepo(t, fmt.Sprintf(`{"name":%q,"package_type":"gems","repo_type":"local"}`, name))
	defer deleteRepo(t, name)

	src := strings.TrimRight(internal, "/") + "/api/v1/local/" + name
	script := `set -euo pipefail
export GEM_HOST_API_KEY="$TOKEN"
mk() { d=/w/src-$1${2:+-b}; mkdir -p $d/lib; cd $d
  echo "module MyorgE2eHello; VERSION = '$1'; end${2:-}" > lib/myorg-e2e-hello.rb
  printf 'Gem::Specification.new { |s| s.name = "myorg-e2e-hello"; s.version = "%s"; s.summary = "e2e"; s.authors = ["e2e"]; s.files = ["lib/myorg-e2e-hello.rb"] }' "$1" > g.gemspec
  gem build g.gemspec >/dev/null 2>&1; }
mk 0.1.0 && gem push --host "$SRC" myorg-e2e-hello-0.1.0.gem
mk 0.2.0 && gem push --host "$SRC" myorg-e2e-hello-0.2.0.gem
if gem push --host "$SRC" myorg-e2e-hello-0.2.0.gem; then echo "republish accepted"; exit 1; fi
mkdir -p /w/app && cd /w/app
printf -- "---\n:sources:\n- $SRC/\n" > ~/.gemrc
gem install --no-document myorg-e2e-hello
ruby -e 'require "myorg-e2e-hello"; puts "GEM #{MyorgE2eHello::VERSION}"'
gem uninstall -a -x myorg-e2e-hello >/dev/null
printf 'source "%s/"\ngem "myorg-e2e-hello"\n' "$SRC" > Gemfile
bundle install
bundle exec ruby -e 'require "myorg-e2e-hello"; puts "BUNDLE #{MyorgE2eHello::VERSION}"'
gem yank myorg-e2e-hello -v 0.2.0 --host "$SRC"
echo "INFO $(curl -s "$SRC/info/myorg-e2e-hello" | cut -d' ' -f1 | tr '\n' ' ')"
mk 0.2.0 '# changed' && if gem push --host "$SRC" myorg-e2e-hello-0.2.0.gem; then echo "yanked re-push accepted"; exit 1; fi
cd /w/app && gem uninstall -a -x myorg-e2e-hello >/dev/null
if gem install --remote --no-document myorg-e2e-hello -v 0.2.0; then echo "yanked version installed"; exit 1; fi
mk 0.3.0 && if GEM_HOST_API_KEY="$BAD" gem push --host "$SRC" myorg-e2e-hello-0.3.0.gem; then echo "invalid key push accepted"; exit 1; fi
echo "VERSIONS $(curl -s "$SRC/versions" | grep '^myorg-e2e-hello ' | cut -d' ' -f2)"`

	out, err := exec.Command("docker", "run", "--rm", "--network", network,
		"-e", "SRC="+src, "-e", "TOKEN="+authToken, "-e", "BAD="+badToken, "ruby:3.3", "bash", "-c", script).CombinedOutput()
	if err != nil {
		t.Fatalf("gem client run failed: %v\n%s", err, out)
	}
	for _, want := range []string{"GEM 0.2.0", "BUNDLE 0.2.0", "INFO --- 0.1.0 ", "re-pushing a yanked version is not allowed", `{"error":"invalid token"}`, "VERSIONS 0.1.0\n"} {
		if !strings.Contains(string(out), want) {
			t.Fatalf("missing %q in gem output:\n%s", want, out)
		}
	}
}
