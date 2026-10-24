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

// abuildScript builds a real, abuild-signed $N-$V-r0.apk and writes it to stdout.
const abuildScript = `set -eu
apk add -q abuild >&2
adduser -D b && addgroup b abuild
su b -c 'set -eu; abuild-keygen -an >&2; mkdir -p ~/p && cd ~/p
printf "%s\n" "pkgname=$N" "pkgver=$V" pkgrel=0 "pkgdesc=\"e2e\"" "url=\"https://example.com\"" "arch=\"x86_64\"" "license=\"MIT\"" "options=\"!check\"" \
  "package() { mkdir -p \"\$pkgdir/usr/share/$N\"; echo ok > \"\$pkgdir/usr/share/$N/ok\"; }" > APKBUILD
abuild -d >&2 || true'
cat /home/b/packages/*/x86_64/$N-$V-r0.apk`

func buildApk(t *testing.T, name, version string) []byte {
	t.Helper()
	cmd := exec.Command("docker", "run", "--rm", "-e", "N="+name, "-e", "V="+version, "alpine:3.20", "sh", "-c", abuildScript)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil || len(out) == 0 {
		t.Fatalf("abuild %s-%s: %v\n%s", name, version, err, stderr.String())
	}
	return out
}

// TestVirtualAlpineMerge merges two local alpine repos carrying the same
// abuild-built package: the first member wins the duplicate and its download,
// and a real apk installs packages from both members through the virtual.
func TestVirtualAlpineMerge(t *testing.T) {
	network := os.Getenv("COMPOSE_NETWORK")
	internal := os.Getenv("ARTIFACTAPI_INTERNAL")
	if network == "" || internal == "" {
		t.Skip("COMPOSE_NETWORK/ARTIFACTAPI_INTERNAL not set; skipping real apk")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not available on the test host")
	}
	one, two := buildApk(t, "vapk-one", "1.0"), buildApk(t, "vapk-two", "2.0")

	createRepo(t, `{"name":"vapk-a","package_type":"alpine","repo_type":"local"}`)
	createRepo(t, `{"name":"vapk-b","package_type":"alpine","repo_type":"local"}`)
	defer deleteRepo(t, "vapk-a")
	defer deleteRepo(t, "vapk-b")

	const apkType = "application/vnd.android.package-archive"
	uploadFile(t, "vapk-a", "x86_64/vapk-one-1.0-r0.apk", one, apkType)
	uploadFile(t, "vapk-b", "x86_64/vapk-one-1.0-r0.apk", one, apkType)
	uploadFile(t, "vapk-b", "x86_64/vapk-two-2.0-r0.apk", two, apkType)

	// Local indexes list uploads once their metadata is parsed asynchronously,
	// and the virtual caches its merge, so merge only once both members list all.
	for _, m := range []struct{ repo, pkg string }{{"vapk-a", "P:vapk-one\n"}, {"vapk-b", "P:vapk-two\n"}} {
		var idx string
		for deadline := time.Now().Add(15 * time.Second); !strings.Contains(idx, m.pkg) && time.Now().Before(deadline); time.Sleep(250 * time.Millisecond) {
			_, body := doRequest(t, http.MethodGet, api("/api/v1/local/"+m.repo+"/x86_64/APKINDEX.tar.gz"), nil, "")
			idx = apkIndexText(t, body)
		}
	}

	createVirtual(t, `{"name":"vapk","package_type":"alpine","members":["vapk-a","vapk-b"]}`)
	defer deleteVirtual(t, "vapk")

	resp, body := doRequest(t, http.MethodGet, api("/api/v1/virtual/vapk/x86_64/APKINDEX.tar.gz"), nil, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("virtual APKINDEX: status %d: %s", resp.StatusCode, body)
	}
	index := apkIndexText(t, body)
	if strings.Count(index, "P:vapk-one\n") != 1 || !strings.Contains(index, "P:vapk-two\n") {
		t.Fatalf("merged APKINDEX should hold vapk-one once and vapk-two:\n%s", index)
	}

	noRedirect := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	for pkg, member := range map[string]string{"vapk-one-1.0-r0.apk": "vapk-a", "vapk-two-2.0-r0.apk": "vapk-b"} {
		resp, err := noRedirect.Get(api("/api/v1/virtual/vapk/x86_64/" + pkg))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if loc := resp.Header.Get("Location"); resp.StatusCode != http.StatusFound || !strings.HasSuffix(loc, "/api/v1/local/"+member+"/x86_64/"+pkg) {
			t.Fatalf("%s: want 302 to %s, got %d %q", pkg, member, resp.StatusCode, loc)
		}
	}

	script := "set -eu; " +
		"echo \"$URL\" > /etc/apk/repositories; " +
		"apk update --allow-untrusted; " +
		"apk add --allow-untrusted vapk-one vapk-two; " +
		"test -f /usr/share/vapk-one/ok && test -f /usr/share/vapk-two/ok"
	out, err := exec.Command("docker", "run", "--rm", "--network", network,
		"-e", "URL="+strings.TrimRight(internal, "/")+"/api/v1/virtual/vapk",
		"alpine:3.20", "sh", "-c", script).CombinedOutput()
	if err != nil {
		t.Fatalf("real apk against alpine virtual failed: %v\n%s", err, out)
	}
}
