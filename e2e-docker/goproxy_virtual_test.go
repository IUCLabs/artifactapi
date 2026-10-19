//go:build dockere2e

package e2edocker

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/mod/module"
	modzip "golang.org/x/mod/zip"
)

// TestVirtualGoProxy publishes to two local goproxy repos and drives a stock go
// client through a virtual over both: merged versions, member order winning a
// duplicate version, and version files redirected to the owning member.
func TestVirtualGoProxy(t *testing.T) {
	network := os.Getenv("COMPOSE_NETWORK")
	internal := os.Getenv("ARTIFACTAPI_INTERNAL")
	if network == "" || internal == "" {
		t.Skip("COMPOSE_NETWORK/ARTIFACTAPI_INTERNAL not set; run via scripts/docker-e2e.sh")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not available on the test host")
	}

	id := time.Now().UnixNano()
	a, b, virt := fmt.Sprintf("e2e-go-a-%d", id), fmt.Sprintf("e2e-go-b-%d", id), fmt.Sprintf("e2e-go-v-%d", id)
	for _, name := range []string{a, b} {
		createRepo(t, fmt.Sprintf(`{"name":%q,"package_type":"goproxy","repo_type":"local"}`, name))
		defer deleteRepo(t, name)
	}
	createVirtual(t, fmt.Sprintf(`{"name":%q,"package_type":"goproxy","members":[%q,%q]}`, virt, a, b))
	defer deleteVirtual(t, virt)

	const mod = "myorg.example.net/hello"
	gomod := []byte("module " + mod + "\n\ngo 1.25\n")
	for _, p := range [][2]string{{a, "v0.1.0"}, {b, "v0.1.0"}, {b, "v0.2.0"}} {
		dir := t.TempDir()
		os.WriteFile(filepath.Join(dir, "go.mod"), gomod, 0o600)
		os.WriteFile(filepath.Join(dir, "hello.go"), []byte("package hello\n\nconst Greeting = \""+p[0]+":"+p[1]+"\"\n"), 0o600)
		var buf bytes.Buffer
		if err := modzip.CreateFromDir(&buf, module.Version{Path: mod, Version: p[1]}, dir); err != nil {
			t.Fatal(err)
		}
		uploadFile(t, p[0], mod+"/@v/"+p[1]+".zip", buf.Bytes(), "application/zip")
		uploadFile(t, p[0], mod+"/@v/"+p[1]+".mod", gomod, "text/plain")
	}

	base := strings.TrimRight(internal, "/")
	script := `set -euo pipefail
export GOPROXY="$BASE/api/v1/virtual/$V" GONOSUMDB=myorg.example.net GOFLAGS=-mod=mod GOTOOLCHAIN=local
mkdir -p /w && cd /w
printf 'module consumer\n\ngo 1.25\n' > go.mod
printf 'package main\n\nimport "myorg.example.net/hello"\n\nfunc main() { println("RUN " + hello.Greeting) }\n' > main.go
echo "VERSIONS $(go list -m -versions myorg.example.net/hello)"
go get myorg.example.net/hello@latest && go run . 2>&1
go get myorg.example.net/hello@v0.1.0 && go run . 2>&1
echo "REDIRECT $(curl -s -o /dev/null -w '%{redirect_url}' "$GOPROXY/myorg.example.net/hello/@v/v0.1.0.zip")"`

	out, err := exec.Command("docker", "run", "--rm", "--network", network,
		"-e", "BASE="+base, "-e", "V="+virt, "golang:1.25", "bash", "-c", script).CombinedOutput()
	if err != nil {
		t.Fatalf("go run failed: %v\n%s", err, out)
	}
	for _, want := range []string{
		"VERSIONS myorg.example.net/hello v0.1.0 v0.2.0",
		"RUN " + b + ":v0.2.0",
		"RUN " + a + ":v0.1.0",
		"REDIRECT " + base + "/api/v1/local/" + a + "/myorg.example.net/hello/@v/v0.1.0.zip",
	} {
		if !strings.Contains(string(out), want) {
			t.Fatalf("missing %q in go output:\n%s", want, out)
		}
	}
}
