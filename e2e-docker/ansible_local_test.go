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

// ansibleHelloScript builds myorg.e2e_hello 1.0.0 with one module into /w/dist.
const ansibleHelloScript = `pip install -q ansible-core >/dev/null 2>&1
mkdir -p /w && cd /w && ansible-galaxy collection init myorg.e2e_hello >/dev/null
cd myorg/e2e_hello && sed -i 's/^version: .*/version: 1.0.0/' galaxy.yml && mkdir -p plugins/modules
printf '%s\n' 'DOCUMENTATION = r"""' 'module: hello' 'short_description: e2e greeting' 'description: Returns a greeting.' 'options:' '  name: {type: str, default: world, description: Who to greet.}' '"""' \
  'from ansible.module_utils.basic import AnsibleModule' \
  'm = AnsibleModule(argument_spec=dict(name=dict(type="str", default="world")))' \
  'm.exit_json(changed=False, msg="hello " + m.params["name"])' > plugins/modules/hello.py
ansible-galaxy collection build --output-path /w/dist >/dev/null
`

// ansibleHelloPlay runs myorg.e2e_hello.hello from collections in /w/coll.
const ansibleHelloPlay = `export ANSIBLE_COLLECTIONS_PATH=/w/coll
ansible-doc myorg.e2e_hello.hello | grep -q 'Returns a greeting'
printf -- '- hosts: localhost\n  gather_facts: false\n  tasks:\n    - myorg.e2e_hello.hello: {name: e2e}\n      register: r\n    - debug: {msg: "PLAY {{ r.msg }}"}\n' > /w/play.yml
ansible-playbook -i localhost, -c local /w/play.yml | grep -o 'PLAY hello e2e' | head -1
`

// TestLocalAnsiblePublishInstall builds a collection with a stock
// ansible-galaxy client, publishes it to a local ansible repo, rejects the
// republish, installs it back and runs its module.
func TestLocalAnsiblePublishInstall(t *testing.T) {
	network := os.Getenv("COMPOSE_NETWORK")
	internal := os.Getenv("ARTIFACTAPI_INTERNAL")
	if network == "" || internal == "" {
		t.Skip("COMPOSE_NETWORK/ARTIFACTAPI_INTERNAL not set; run via scripts/docker-e2e.sh")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not available on the test host")
	}

	name := fmt.Sprintf("e2e-ansible-local-%d", time.Now().UnixNano())
	createRepo(t, fmt.Sprintf(`{"name":%q,"package_type":"ansible","repo_type":"local"}`, name))
	defer deleteRepo(t, name)

	script := "set -euo pipefail\n" + ansibleHelloScript + `
ansible-galaxy collection publish /w/dist/myorg-e2e_hello-1.0.0.tar.gz --server "$SRV" --api-key unused
if ansible-galaxy collection publish /w/dist/myorg-e2e_hello-1.0.0.tar.gz --server "$SRV" --api-key unused; then echo "republish accepted"; exit 1; fi
ansible-galaxy collection install myorg.e2e_hello:==1.0.0 --server "$SRV" -p /w/coll
` + ansibleHelloPlay

	srv := strings.TrimRight(internal, "/") + "/api/v1/local/" + name + "/"
	out, err := exec.Command("docker", "run", "--rm", "--network", network,
		"-e", "SRV="+srv, "python:3.12-slim", "bash", "-c", script).CombinedOutput()
	if err != nil {
		t.Fatalf("ansible-galaxy client run failed: %v\n%s", err, out)
	}
	for _, want := range []string{"successfully published and imported", "HTTP Code: 409", "myorg.e2e_hello:1.0.0 was installed successfully", "PLAY hello e2e"} {
		if !strings.Contains(string(out), want) {
			t.Fatalf("missing %q in client output:\n%s", want, out)
		}
	}
}
