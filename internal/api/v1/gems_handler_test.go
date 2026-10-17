package v1

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/md5"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/IUCLabs/artifactapi/pkg/models"
)

func testGemFile(t *testing.T, name, version, platform string) []byte {
	t.Helper()
	meta := fmt.Sprintf(`--- !ruby/object:Gem::Specification
name: %s
version: !ruby/object:Gem::Version
  version: %s
platform: %s
dependencies:
- !ruby/object:Gem::Dependency
  name: rack
  requirement: !ruby/object:Gem::Requirement
    requirements:
    - - "~>"
      - !ruby/object:Gem::Version
        version: '3.0'
  type: :runtime
  prerelease: false
rubygems_version: 3.5.22
specification_version: 4
summary: test
`, name, version, platform)
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	_, _ = zw.Write([]byte(meta))
	_ = zw.Close()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	_ = tw.WriteHeader(&tar.Header{Name: "metadata.gz", Mode: 0o444, Size: int64(gz.Len())})
	_, _ = tw.Write(gz.Bytes())
	_ = tw.Close()
	return buf.Bytes()
}

func sha256Hex(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

func TestGemsPushIndexYank(t *testing.T) {
	h, srv := npmTestServer(t)
	repo := npmRepo(t, h, models.PackageGems)
	base := srv.URL + "/api/v1/local/" + repo + "/"

	g1 := testGemFile(t, "hello_gem", "0.1.0", "ruby")
	g2 := testGemFile(t, "hello_gem", "0.2.0", "ruby")
	gj := testGemFile(t, "hello_gem", "0.2.0", "java")
	other := testGemFile(t, "hello-gem", "1.0.0", "ruby")
	for _, g := range [][]byte{g1, g2, gj, other} {
		if code, body := npmDo(t, http.MethodPost, base+"api/v1/gems", string(g)); code != http.StatusOK {
			t.Fatalf("push = %d: %s", code, body)
		}
	}
	if code, _ := npmDo(t, http.MethodPost, base+"api/v1/gems", string(g1)+"x"); code != http.StatusConflict {
		t.Fatalf("repush with different bytes = %d, want 409", code)
	}
	if code, _ := npmDo(t, http.MethodPost, base+"api/v1/gems", string(g1)); code != http.StatusConflict {
		t.Fatalf("identical repush = %d, want 409", code)
	}
	if code, _ := npmDo(t, http.MethodPost, base+"api/v1/gems", "not a gem"); code != http.StatusBadRequest {
		t.Fatalf("garbage push = %d, want 400", code)
	}

	code, info := npmDo(t, http.MethodGet, base+"info/hello_gem", "")
	want := "---\n0.1.0 rack:~> 3.0|checksum:" + sha256Hex(g1) + "\n0.2.0 rack:~> 3.0|checksum:" + sha256Hex(g2) +
		"\n0.2.0-java rack:~> 3.0|checksum:" + sha256Hex(gj) + "\n"
	if code != http.StatusOK || info != want {
		t.Fatalf("info = %d\n%q\nwant\n%q", code, info, want)
	}
	if code, _ := npmDo(t, http.MethodHead, base+"versions", ""); code != http.StatusOK {
		t.Fatalf("HEAD versions = %d; gem probes the compact index with HEAD", code)
	}
	_, versions := npmDo(t, http.MethodGet, base+"versions", "")
	head, body, ok := strings.Cut(versions, "\n---\n")
	if !ok || !strings.HasPrefix(head, "created_at: ") {
		t.Fatalf("versions header = %q", versions)
	}
	_, otherInfo := npmDo(t, http.MethodGet, base+"info/hello-gem", "")
	wantBody := fmt.Sprintf("hello-gem 1.0.0 %x\nhello_gem 0.1.0,0.2.0,0.2.0-java %x\n", md5.Sum([]byte(otherInfo)), md5.Sum([]byte(info)))
	if body != wantBody {
		t.Fatalf("versions body =\n%q\nwant\n%q", body, wantBody)
	}
	if _, names := npmDo(t, http.MethodGet, base+"names", ""); names != "---\nhello-gem\nhello_gem\n" {
		t.Fatalf("names = %q", names)
	}
	if code, got := npmDo(t, http.MethodGet, base+"gems/hello_gem-0.2.0-java.gem", ""); code != http.StatusOK || got != string(gj) {
		t.Fatalf("download = %d", code)
	}
	if code, _ := npmDo(t, http.MethodGet, base+"quick/Marshal.4.8/hello_gem-0.1.0.gemspec.rz", ""); code != http.StatusOK {
		t.Fatalf("quick spec = %d", code)
	}
	if code, _ := npmDo(t, http.MethodGet, base+"info/missing", ""); code != http.StatusNotFound {
		t.Fatalf("missing info = %d", code)
	}

	yank := func(form string) int {
		req, _ := http.NewRequest(http.MethodDelete, base+"api/v1/gems/yank", strings.NewReader(form))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		return resp.StatusCode
	}
	if c := yank("gem_name=hello_gem&version=0.2.0&platform=java"); c != http.StatusOK {
		t.Fatalf("yank = %d", c)
	}
	if c := yank("gem_name=hello_gem&version=0.2.0&platform=java"); c != http.StatusNotFound {
		t.Fatalf("second yank = %d, want 404", c)
	}
	if c := yank("gem_name=../x&version=1"); c != http.StatusBadRequest {
		t.Fatalf("bad yank = %d, want 400", c)
	}
	if _, info := npmDo(t, http.MethodGet, base+"info/hello_gem", ""); strings.Contains(info, "java") || !strings.Contains(info, "0.2.0 ") {
		t.Fatalf("info after yank = %q", info)
	}
	if code, _ := npmDo(t, http.MethodGet, base+"gems/hello_gem-0.2.0-java.gem", ""); code != http.StatusNotFound {
		t.Fatalf("yanked download = %d, want 404", code)
	}
	if code, _ := npmDo(t, http.MethodPost, base+"api/v1/gems", string(gj)); code != http.StatusOK {
		t.Fatalf("push after yank = %d", code)
	}
}

func TestGemsPushRejects(t *testing.T) {
	h, srv := npmTestServer(t)
	npmLocal := npmRepo(t, h, models.PackageNPM)
	g := string(testGemFile(t, "g", "1.0.0", "ruby"))
	if code, _ := npmDo(t, http.MethodPost, srv.URL+"/api/v1/local/"+npmLocal+"/api/v1/gems", g); code != http.StatusMethodNotAllowed {
		t.Fatalf("push to npm local = %d, want 405", code)
	}
	repo := npmRepo(t, h, models.PackageGems)
	if code, _ := npmDo(t, http.MethodPost, srv.URL+"/api/v1/local/"+repo+"/other", g); code != http.StatusNotFound {
		t.Fatalf("push to other path = %d, want 404", code)
	}
	old := gemsMaxPushBytes
	gemsMaxPushBytes = 64
	defer func() { gemsMaxPushBytes = old }()
	if code, _ := npmDo(t, http.MethodPost, srv.URL+"/api/v1/local/"+repo+"/api/v1/gems", g); code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized push = %d, want 413", code)
	}
}
