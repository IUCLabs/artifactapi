//go:build dockere2e

package e2edocker

import (
	"bytes"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/mod/module"
	modzip "golang.org/x/mod/zip"
)

// TestLocalGoProxy publishes two module versions and reads them back over the
// GOPROXY protocol.
func TestLocalGoProxy(t *testing.T) {
	createRepo(t, `{"name":"local-go","package_type":"goproxy","repo_type":"local"}`)
	defer deleteRepo(t, "local-go")

	const mod = "myorg.example.net/!hello"
	gomod := []byte("module myorg.example.net/Hello\n\ngo 1.25\n")
	var zip010 []byte
	for _, ver := range []string{"v0.1.0", "v0.2.0"} {
		dir := t.TempDir()
		os.WriteFile(filepath.Join(dir, "go.mod"), gomod, 0o600)
		os.WriteFile(filepath.Join(dir, "hello.go"), []byte("package hello\n"), 0o600)
		var buf bytes.Buffer
		if err := modzip.CreateFromDir(&buf, module.Version{Path: "myorg.example.net/Hello", Version: ver}, dir); err != nil {
			t.Fatal(err)
		}
		if ver == "v0.1.0" {
			zip010 = buf.Bytes()
		}
		uploadFile(t, "local-go", mod+"/@v/"+ver+".zip", buf.Bytes(), "application/zip")
		uploadFile(t, "local-go", mod+"/@v/"+ver+".mod", gomod, "text/plain")
	}

	assertWriteGated(t, http.MethodPut, api("/api/v2/remotes/local-go/files/"+mod+"/@v/v0.3.0.mod"), gomod, "text/plain", api("/api/v1/local/local-go/"+mod+"/@v/list"))

	resp, body := doRequest(t, http.MethodPut, api("/api/v2/remotes/local-go/files/"+mod+"/@v/v0.3.0.zip"), []byte("junk"), "application/zip")
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("invalid zip: status %d: %s", resp.StatusCode, body)
	}

	for path, want := range map[string]string{
		"/@v/list":        "v0.1.0\nv0.2.0\n",
		"/@latest":        `{"Version":"v0.2.0"}`,
		"/@v/v0.1.0.info": `{"Version":"v0.1.0"}`,
		"/@v/v0.1.0.mod":  string(gomod),
		"/@v/v0.1.0.zip":  string(zip010),
	} {
		resp, body := doRequest(t, http.MethodGet, api("/api/v1/local/local-go/"+mod+path), nil, "")
		if resp.StatusCode != http.StatusOK || string(body) != want {
			t.Errorf("%s: status %d body %q", path, resp.StatusCode, body)
		}
	}
}
