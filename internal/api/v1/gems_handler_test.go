package v1

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/md5"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/IUCLabs/artifactapi/internal/cache"
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
	if _, v := npmDo(t, http.MethodGet, base+"versions", ""); strings.Contains(v, "java") || !strings.Contains(v, "hello_gem 0.1.0,0.2.0 ") {
		t.Fatalf("versions after yank = %q", v)
	}
	for _, g := range []string{string(gj), string(gj) + "x"} {
		if code, body := npmDo(t, http.MethodPost, base+"api/v1/gems", g); code != http.StatusConflict || !strings.Contains(body, "yanked") {
			t.Fatalf("push after yank = %d %q, want 409", code, body)
		}
	}
	if _, info := npmDo(t, http.MethodGet, base+"info/hello_gem", ""); strings.Contains(info, "java") {
		t.Fatalf("info after rejected re-push = %q", info)
	}

	if c := yank("gem_name=hello-gem&version=1.0.0"); c != http.StatusOK {
		t.Fatalf("yank last version = %d", c)
	}
	if code, _ := npmDo(t, http.MethodGet, base+"info/hello-gem", ""); code != http.StatusNotFound {
		t.Fatalf("info for fully yanked gem = %d, want 404", code)
	}
	if _, names := npmDo(t, http.MethodGet, base+"names", ""); names != "---\nhello_gem\n" {
		t.Fatalf("names after yanking every version = %q", names)
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

func TestGemsPushYankSerialised(t *testing.T) {
	h, srv := npmTestServer(t)
	repo := npmRepo(t, h, models.PackageGems)
	base := srv.URL + "/api/v1/local/" + repo + "/"
	g := string(testGemFile(t, "race", "1.0.0", "ruby"))
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			if code, body := npmDo(t, http.MethodPost, base+"api/v1/gems", g); code != http.StatusOK && code != http.StatusConflict {
				t.Errorf("push = %d: %s", code, body)
			}
		}()
		go func() {
			defer wg.Done()
			if code, body := npmDo(t, http.MethodDelete, base+"api/v1/gems/yank?gem_name=race&version=1.0.0", ""); code != http.StatusOK && code != http.StatusNotFound {
				t.Errorf("yank = %d: %s", code, body)
			}
		}()
	}
	wg.Wait()

	present := func(p string) bool {
		f, err := h.db.GetLocalFile(context.Background(), repo, p)
		if err != nil {
			t.Fatal(err)
		}
		return f != nil
	}
	live := []string{"_index/race/race-1.0.0", "gems/race-1.0.0.gem", "quick/Marshal.4.8/race-1.0.0.gemspec.rz"}
	tomb := present("_yanked/race/race-1.0.0")
	for _, p := range live {
		if present(p) == tomb {
			t.Errorf("tombstoned=%v but %s present=%v", tomb, p, !tomb)
		}
	}
	_, info := npmDo(t, http.MethodGet, base+"info/race", "")
	if strings.Contains(info, "1.0.0") == tomb {
		t.Errorf("tombstoned=%v but info = %q", tomb, info)
	}
}

func TestGemsLockTimeoutIs503(t *testing.T) {
	h, srv := npmTestServer(t)
	repo := npmRepo(t, h, models.PackageGems)
	base := srv.URL + "/api/v1/local/" + repo + "/"
	redis, err := cache.NewRedis(testRedis)
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := redis.AcquireLock(context.Background(), repo, "_gemlock/busy-1.0.0", time.Minute); !ok || err != nil {
		t.Fatalf("hold lock = %v %v", ok, err)
	}
	prev := gemsLockWait
	gemsLockWait = 100 * time.Millisecond
	t.Cleanup(func() { gemsLockWait = prev })

	if code, _ := npmDo(t, http.MethodPost, base+"api/v1/gems", string(testGemFile(t, "busy", "1.0.0", "ruby"))); code != http.StatusServiceUnavailable {
		t.Fatalf("push under held lock = %d, want 503", code)
	}
	if code, _ := npmDo(t, http.MethodDelete, base+"api/v1/gems/yank?gem_name=busy&version=1.0.0", ""); code != http.StatusServiceUnavailable {
		t.Fatalf("yank under held lock = %d, want 503", code)
	}
	if err := redis.ReleaseLock(context.Background(), repo, "_gemlock/busy-1.0.0"); err != nil {
		t.Fatal(err)
	}
	if code, body := npmDo(t, http.MethodPost, base+"api/v1/gems", string(testGemFile(t, "busy", "1.0.0", "ruby"))); code != http.StatusOK {
		t.Fatalf("push after release = %d: %s", code, body)
	}
}
