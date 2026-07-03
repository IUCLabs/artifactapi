package server

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/IUCLabs/artifactapi/internal/config"
	"github.com/IUCLabs/artifactapi/internal/testsupport"
)

var (
	testTS   *httptest.Server // the artifactapi router
	upstream *httptest.Server // mock upstream the proxy fetches from
	testSrv  *Server
)

func TestMain(m *testing.M) {
	ctx := context.Background()

	dsn, termPG, err := testsupport.StartPostgres(ctx)
	if err != nil {
		os.Exit(m.Run())
	}
	defer termPG()
	redisURL, termRedis, err := testsupport.StartRedis(ctx)
	if err != nil {
		termPG()
		os.Exit(m.Run())
	}
	defer termRedis()
	minio, termMinio, err := testsupport.StartMinio(ctx)
	if err != nil {
		termPG()
		termRedis()
		os.Exit(m.Run())
	}
	defer termMinio()

	u, _ := url.Parse(dsn)
	port, _ := strconv.Atoi(u.Port())
	cfg := &config.Config{
		ListenAddr:  ":0",
		DBHost:      u.Hostname(),
		DBPort:      port,
		DBUser:      "artifacts",
		DBPass:      "artifacts123",
		DBName:      "artifacts",
		DBSSL:       "disable",
		RedisURL:    redisURL,
		S3Endpoint:  minio.Endpoint,
		S3AccessKey: minio.AccessKey,
		S3SecretKey: minio.SecretKey,
		S3Bucket:    "server-test",
	}

	var srv *Server
	for i := 0; i < 20; i++ { // tolerate MinIO reporting ready before bucket ops succeed
		if srv, err = New(cfg, "test-version"); err == nil {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	if err != nil {
		panic(err)
	}
	testSrv = srv
	testTS = httptest.NewServer(srv.router)
	upstream = httptest.NewServer(http.HandlerFunc(mockUpstream))

	code := m.Run()

	testTS.Close()
	upstream.Close()
	termMinio()
	termRedis()
	termPG()
	if code != 0 {
		os.Exit(code)
	}
}

func mockUpstream(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/data/file.bin":
		w.Write([]byte("upstream blob payload"))
	case "/helm-a/index.yaml":
		w.Write([]byte("apiVersion: v1\nentries:\n  alpha:\n    - name: alpha\n      version: 1.0.0\n      urls: [charts/alpha-1.0.0.tgz]\n"))
	case "/helm-b/index.yaml":
		w.Write([]byte("apiVersion: v1\nentries:\n  beta:\n    - name: beta\n      version: 2.0.0\n      urls: [charts/beta-2.0.0.tgz]\n"))
	default:
		http.NotFound(w, r)
	}
}

func requireStack(t *testing.T) {
	t.Helper()
	if testTS == nil {
		t.Skip("Docker unavailable; skipping server integration test")
	}
}

func req(t *testing.T, method, path string, body string) (*http.Response, []byte) {
	t.Helper()
	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	rq, _ := http.NewRequest(method, testTS.URL+path, r)
	if body != "" {
		rq.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(rq)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, b
}

// reqNoRedirect issues a request without following redirects so the response's
// status and Location header can be asserted directly.
func reqNoRedirect(t *testing.T, method, path string) *http.Response {
	t.Helper()
	rq, _ := http.NewRequest(method, testTS.URL+path, nil)
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	resp, err := client.Do(rq)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	resp.Body.Close()
	return resp
}

func TestServerHealthAndRoot(t *testing.T) {
	requireStack(t)
	if resp, _ := req(t, "GET", "/health", ""); resp.StatusCode != 200 {
		t.Errorf("health: %d", resp.StatusCode)
	}
	if resp := reqNoRedirect(t, "GET", "/"); resp.StatusCode != http.StatusFound || resp.Header.Get("Location") != "/ui/" {
		t.Errorf("root redirect: %d %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	if resp, b := req(t, "GET", "/version", ""); resp.StatusCode != 200 || !strings.Contains(string(b), "test-version") {
		t.Errorf("version: %d %s", resp.StatusCode, b)
	}
	if resp, _ := req(t, "GET", "/api/v2/health", ""); resp.StatusCode != 200 {
		t.Errorf("health v2: %d", resp.StatusCode)
	}
}

func TestServerRemoteAndProxy(t *testing.T) {
	requireStack(t)
	create := fmt.Sprintf(`{"name":"srv-remote","package_type":"generic","repo_type":"remote","base_url":%q,"stale_on_error":true}`, upstream.URL)
	if resp, b := req(t, "POST", "/api/v2/remotes", create); resp.StatusCode != 201 {
		t.Fatalf("create remote: %d %s", resp.StatusCode, b)
	}
	defer req(t, "DELETE", "/api/v2/remotes/srv-remote", "")

	if resp, _ := req(t, "GET", "/api/v2/remotes/srv-remote", ""); resp.StatusCode != 200 {
		t.Errorf("get remote: %d", resp.StatusCode)
	}
	if resp, _ := req(t, "GET", "/api/v2/remotes", ""); resp.StatusCode != 200 {
		t.Errorf("list remotes: %d", resp.StatusCode)
	}

	// Proxy fetch: miss then hit.
	resp, b := req(t, "GET", "/api/v1/remote/srv-remote/data/file.bin", "")
	if resp.StatusCode != 200 || string(b) != "upstream blob payload" {
		t.Fatalf("proxy miss: %d %s", resp.StatusCode, b)
	}
	if src := resp.Header.Get("X-Artifact-Source"); src != "remote" {
		t.Errorf("expected remote source, got %q", src)
	}
	resp, _ = req(t, "GET", "/api/v1/remote/srv-remote/data/file.bin", "")
	if resp.Header.Get("X-Artifact-Source") != "cache" {
		t.Errorf("second fetch should be cache: %q", resp.Header.Get("X-Artifact-Source"))
	}

	// Objects listing + stats now that we have an artifact.
	if resp, _ := req(t, "GET", "/api/v2/remotes/srv-remote/objects", ""); resp.StatusCode != 200 {
		t.Errorf("objects: %d", resp.StatusCode)
	}
	if resp, _ := req(t, "GET", "/api/v2/stats", ""); resp.StatusCode != 200 {
		t.Errorf("stats: %d", resp.StatusCode)
	}
	for _, p := range []string{"/api/v2/stats/top-remotes", "/api/v2/stats/top-files-by-hits", "/api/v2/stats/top-files-by-bandwidth"} {
		if resp, _ := req(t, "GET", p, ""); resp.StatusCode != 200 {
			t.Errorf("%s: %d", p, resp.StatusCode)
		}
	}
}

func TestServerLocalUpload(t *testing.T) {
	requireStack(t)
	if resp, b := req(t, "POST", "/api/v2/remotes", `{"name":"srv-local","package_type":"generic","repo_type":"local"}`); resp.StatusCode != 201 {
		t.Fatalf("create local: %d %s", resp.StatusCode, b)
	}
	defer req(t, "DELETE", "/api/v2/remotes/srv-local", "")

	rq, _ := http.NewRequest("PUT", testTS.URL+"/api/v2/remotes/srv-local/files/dir/hello.bin", strings.NewReader("local payload"))
	rq.Header.Set("Content-Type", "text/plain") // exercise the content-type branch
	resp, err := http.DefaultClient.Do(rq)
	if err != nil || resp.StatusCode != 201 {
		t.Fatalf("upload: %v %d", err, resp.StatusCode)
	}
	resp.Body.Close()

	resp, b := req(t, "GET", "/api/v1/local/srv-local/dir/hello.bin", "")
	if resp.StatusCode != 200 || string(b) != "local payload" {
		t.Errorf("download local: %d %s", resp.StatusCode, b)
	}
	// Also download via the v2 files endpoint.
	if resp, b := req(t, "GET", "/api/v2/remotes/srv-local/files/dir/hello.bin", ""); resp.StatusCode != 200 || string(b) != "local payload" {
		t.Errorf("v2 download: %d %s", resp.StatusCode, b)
	}
}

func TestServerVirtualMerge(t *testing.T) {
	requireStack(t)
	for _, m := range []string{"a", "b"} {
		body := fmt.Sprintf(`{"name":"srv-helm-%s","package_type":"helm","repo_type":"remote","base_url":"%s/helm-%s","stale_on_error":true}`, m, upstream.URL, m)
		if resp, b := req(t, "POST", "/api/v2/remotes", body); resp.StatusCode != 201 {
			t.Fatalf("create helm-%s: %d %s", m, resp.StatusCode, b)
		}
		defer req(t, "DELETE", "/api/v2/remotes/srv-helm-"+m, "")
	}
	if resp, b := req(t, "POST", "/api/v2/virtuals", `{"name":"srv-vh","package_type":"helm","members":["srv-helm-a","srv-helm-b"]}`); resp.StatusCode != 201 {
		t.Fatalf("create virtual: %d %s", resp.StatusCode, b)
	}
	defer req(t, "DELETE", "/api/v2/virtuals/srv-vh", "")

	resp, b := req(t, "GET", "/api/v1/virtual/srv-vh/index.yaml", "")
	if resp.StatusCode != 200 {
		t.Fatalf("virtual fetch: %d %s", resp.StatusCode, b)
	}
	s := string(b)
	if !strings.Contains(s, "alpha") || !strings.Contains(s, "beta") {
		t.Errorf("merged index missing charts: %s", s)
	}
}

func TestServerProbe(t *testing.T) {
	requireStack(t)
	create := fmt.Sprintf(`{"name":"srv-probe","package_type":"generic","repo_type":"remote","base_url":%q,"stale_on_error":true}`, upstream.URL)
	req(t, "POST", "/api/v2/remotes", create)
	defer req(t, "DELETE", "/api/v2/remotes/srv-probe", "")

	// Reachable path -> status 200 in the probe body.
	if resp, b := req(t, "POST", "/api/v2/probe", `{"remote":"srv-probe","path":"data/file.bin"}`); resp.StatusCode != 200 || !strings.Contains(string(b), `"status":200`) {
		t.Errorf("probe reachable: %d %s", resp.StatusCode, b)
	}
	// Missing upstream path -> upstream error reported (502) in the body.
	if resp, b := req(t, "POST", "/api/v2/probe", `{"remote":"srv-probe","path":"missing"}`); resp.StatusCode != 200 || !strings.Contains(string(b), `"status":502`) {
		t.Errorf("probe missing: %d %s", resp.StatusCode, b)
	}
	// Unknown remote -> 404 in the body.
	if resp, b := req(t, "POST", "/api/v2/probe", `{"remote":"nope","path":"x"}`); resp.StatusCode != 200 || !strings.Contains(string(b), `"status":404`) {
		t.Errorf("probe unknown: %d %s", resp.StatusCode, b)
	}
	// Bad requests.
	if resp, _ := req(t, "POST", "/api/v2/probe", `{}`); resp.StatusCode != 400 {
		t.Errorf("probe missing fields: %d", resp.StatusCode)
	}
	if resp, _ := req(t, "POST", "/api/v2/probe", `not json`); resp.StatusCode != 400 {
		t.Errorf("probe invalid json: %d", resp.StatusCode)
	}
}

func put(t *testing.T, path string, body []byte) (*http.Response, []byte) {
	t.Helper()
	rq, _ := http.NewRequest("PUT", testTS.URL+path, bytes.NewReader(body))
	resp, err := http.DefaultClient.Do(rq)
	if err != nil {
		t.Fatalf("PUT %s: %v", path, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, b
}

func TestServerLocalPyPI(t *testing.T) {
	requireStack(t)
	if resp, b := req(t, "POST", "/api/v2/remotes", `{"name":"srv-pypi","package_type":"pypi","repo_type":"local"}`); resp.StatusCode != 201 {
		t.Fatalf("create pypi local: %d %s", resp.StatusCode, b)
	}
	defer req(t, "DELETE", "/api/v2/remotes/srv-pypi", "")

	if resp, b := put(t, "/api/v2/remotes/srv-pypi/files/foo-1.0-py3-none-any.whl", []byte("wheel bytes")); resp.StatusCode != 201 {
		t.Fatalf("upload wheel: %d %s", resp.StatusCode, b)
	}
	// Re-uploading the same file is rejected.
	if resp, _ := put(t, "/api/v2/remotes/srv-pypi/files/foo-1.0-py3-none-any.whl", []byte("again")); resp.StatusCode != 409 {
		t.Errorf("expected 409 on overwrite, got %d", resp.StatusCode)
	}
	// Invalid pypi filename rejected.
	if resp, _ := put(t, "/api/v2/remotes/srv-pypi/files/not-a-package.txt", []byte("x")); resp.StatusCode != 400 {
		t.Errorf("expected 400 for bad filename, got %d", resp.StatusCode)
	}

	if resp, b := req(t, "GET", "/api/v1/local/srv-pypi/simple/", ""); resp.StatusCode != 200 || !strings.Contains(string(b), "foo") {
		t.Errorf("simple index: %d %s", resp.StatusCode, b)
	}
	if resp, b := req(t, "GET", "/api/v1/local/srv-pypi/simple/foo/", ""); resp.StatusCode != 200 || !strings.Contains(string(b), "foo-1.0-py3-none-any.whl") {
		t.Errorf("package index: %d %s", resp.StatusCode, b)
	}
}

func TestServerLocalRPMRepodata(t *testing.T) {
	requireStack(t)
	rpm := testsupport.MinimalRPM("e2e-testpkg", "1.0", "1", "noarch")
	if resp, b := req(t, "POST", "/api/v2/remotes", `{"name":"srv-rpm","package_type":"rpm","repo_type":"local"}`); resp.StatusCode != 201 {
		t.Fatalf("create rpm local: %d %s", resp.StatusCode, b)
	}
	defer req(t, "DELETE", "/api/v2/remotes/srv-rpm", "")

	if resp, b := put(t, "/api/v2/remotes/srv-rpm/files/e2e-testpkg-1.0-1.noarch.rpm", rpm); resp.StatusCode != 201 {
		t.Fatalf("upload rpm: %d %s", resp.StatusCode, b)
	}

	// repodata is generated asynchronously; poll for it.
	var body []byte
	for i := 0; i < 40; i++ {
		var resp *http.Response
		resp, body = req(t, "GET", "/api/v1/local/srv-rpm/repodata/repomd.xml", "")
		if resp.StatusCode == 200 && strings.Contains(string(body), "<repomd") {
			return
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Errorf("repomd.xml not generated: %s", body)
}

func TestServerObjectEviction(t *testing.T) {
	requireStack(t)
	create := fmt.Sprintf(`{"name":"srv-evict","package_type":"generic","repo_type":"remote","base_url":%q,"stale_on_error":true}`, upstream.URL)
	req(t, "POST", "/api/v2/remotes", create)
	defer req(t, "DELETE", "/api/v2/remotes/srv-evict", "")

	resp, _ := req(t, "GET", "/api/v1/remote/srv-evict/data/file.bin", "")
	resp.Body.Close()
	if resp, _ := req(t, "DELETE", "/api/v2/remotes/srv-evict/objects/data/file.bin", ""); resp.StatusCode >= 400 {
		t.Errorf("evict object: %d", resp.StatusCode)
	}
}

func TestServerValidationErrors(t *testing.T) {
	requireStack(t)
	if resp, _ := req(t, "POST", "/api/v2/remotes", `{"name":"bad","package_type":"bogus","base_url":"https://x"}`); resp.StatusCode != 400 {
		t.Errorf("invalid package type: %d", resp.StatusCode)
	}
	if resp, _ := req(t, "POST", "/api/v2/remotes", `{"name":"bad","package_type":"generic","repo_type":"remote"}`); resp.StatusCode != 400 {
		t.Errorf("missing base_url: %d", resp.StatusCode)
	}
	if resp, _ := req(t, "POST", "/api/v2/remotes", `not json`); resp.StatusCode != 400 {
		t.Errorf("invalid json: %d", resp.StatusCode)
	}
	// Invalid regex pattern -> 400 from ValidatePatterns.
	if resp, _ := req(t, "POST", "/api/v2/remotes", `{"name":"badre","package_type":"generic","repo_type":"remote","base_url":"https://x","blocklist":["[unterminated"]}`); resp.StatusCode != 400 {
		t.Errorf("invalid regex: %d", resp.StatusCode)
	}
}

func TestServerDockerAndHead(t *testing.T) {
	requireStack(t)
	create := fmt.Sprintf(`{"name":"srv-docker","package_type":"generic","repo_type":"remote","base_url":%q,"stale_on_error":true}`, upstream.URL)
	req(t, "POST", "/api/v2/remotes", create)
	defer req(t, "DELETE", "/api/v2/remotes/srv-docker", "")

	// Docker registry ping.
	if resp, _ := req(t, "GET", "/v2/", ""); resp.StatusCode != 200 {
		t.Errorf("docker ping: %d", resp.StatusCode)
	}
	// HEAD through the docker route resolves metadata (uncached -> upstream).
	rq, _ := http.NewRequest("HEAD", testTS.URL+"/v2/srv-docker/data/file.bin", nil)
	resp, err := http.DefaultClient.Do(rq)
	if err != nil {
		t.Fatalf("head: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("head status: %d", resp.StatusCode)
	}
}

func TestServerRemoteUpdateAndVirtualCRUD(t *testing.T) {
	requireStack(t)
	req(t, "POST", "/api/v2/remotes", `{"name":"srv-upd","package_type":"helm","repo_type":"remote","base_url":"https://a.example.com","stale_on_error":true}`)
	defer req(t, "DELETE", "/api/v2/remotes/srv-upd", "")
	if resp, b := req(t, "PUT", "/api/v2/remotes/srv-upd", `{"package_type":"helm","base_url":"https://b.example.com","stale_on_error":true}`); resp.StatusCode != 200 {
		t.Errorf("update remote: %d %s", resp.StatusCode, b)
	}

	req(t, "POST", "/api/v2/virtuals", `{"name":"srv-v2","package_type":"helm","members":["srv-upd"]}`)
	defer req(t, "DELETE", "/api/v2/virtuals/srv-v2", "")
	if resp, _ := req(t, "GET", "/api/v2/virtuals/srv-v2", ""); resp.StatusCode != 200 {
		t.Errorf("get virtual: %d", resp.StatusCode)
	}
	if resp, _ := req(t, "GET", "/api/v2/virtuals", ""); resp.StatusCode != 200 {
		t.Errorf("list virtuals: %d", resp.StatusCode)
	}
	if resp, b := req(t, "PUT", "/api/v2/virtuals/srv-v2", `{"package_type":"helm","members":["srv-upd"]}`); resp.StatusCode != 200 {
		t.Errorf("update virtual: %d %s", resp.StatusCode, b)
	}
}

func TestServerLocalRemoveAndMissing(t *testing.T) {
	requireStack(t)
	req(t, "POST", "/api/v2/remotes", `{"name":"srv-rm","package_type":"generic","repo_type":"local"}`)
	defer req(t, "DELETE", "/api/v2/remotes/srv-rm", "")

	put(t, "/api/v2/remotes/srv-rm/files/a/b.bin", []byte("payload"))
	if resp, _ := req(t, "DELETE", "/api/v2/remotes/srv-rm/files/a/b.bin", ""); resp.StatusCode >= 400 {
		t.Errorf("delete local file: %d", resp.StatusCode)
	}
	if resp, _ := req(t, "GET", "/api/v1/local/srv-rm/a/b.bin", ""); resp.StatusCode != 404 {
		t.Errorf("expected 404 for removed file, got %d", resp.StatusCode)
	}
}

func TestServerLocalUploadErrors(t *testing.T) {
	requireStack(t)
	// Uploading to a remote-type repo is rejected.
	create := fmt.Sprintf(`{"name":"srv-uerr","package_type":"generic","repo_type":"remote","base_url":%q,"stale_on_error":true}`, upstream.URL)
	req(t, "POST", "/api/v2/remotes", create)
	defer req(t, "DELETE", "/api/v2/remotes/srv-uerr", "")
	if resp, _ := put(t, "/api/v2/remotes/srv-uerr/files/x.bin", []byte("x")); resp.StatusCode != 400 {
		t.Errorf("upload to remote repo should be 400, got %d", resp.StatusCode)
	}

	// Duplicate generic upload is a conflict.
	req(t, "POST", "/api/v2/remotes", `{"name":"srv-dup","package_type":"generic","repo_type":"local"}`)
	defer req(t, "DELETE", "/api/v2/remotes/srv-dup", "")
	put(t, "/api/v2/remotes/srv-dup/files/dup.bin", []byte("one"))
	if resp, _ := put(t, "/api/v2/remotes/srv-dup/files/dup.bin", []byte("two")); resp.StatusCode != 409 {
		t.Errorf("duplicate upload should be 409, got %d", resp.StatusCode)
	}

	// Download of a missing local file is 404.
	if resp, _ := req(t, "GET", "/api/v1/local/srv-dup/does/not/exist", ""); resp.StatusCode != 404 {
		t.Errorf("missing local download should be 404, got %d", resp.StatusCode)
	}
	// Unknown virtual is 404.
	if resp, _ := req(t, "GET", "/api/v1/virtual/nope/index.yaml", ""); resp.StatusCode != 404 {
		t.Errorf("unknown virtual should be 404, got %d", resp.StatusCode)
	}
}

func TestServerEvents(t *testing.T) {
	requireStack(t)
	client := &http.Client{Timeout: 800 * time.Millisecond}
	resp, err := client.Get(testTS.URL + "/api/v2/events")
	if err == nil {
		resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Errorf("events status: %d", resp.StatusCode)
		}
	}
	// A timeout is expected for a streaming endpoint; the handler still ran.
}

func TestRunOnListener(t *testing.T) {
	requireStack(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() { errc <- testSrv.RunOnListener(ctx, ln) }()

	base := "http://" + ln.Addr().String()
	ok := false
	for i := 0; i < 50; i++ {
		if resp, e := http.Get(base + "/health"); e == nil {
			resp.Body.Close()
			ok = resp.StatusCode == 200
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !ok {
		t.Error("server did not serve /health")
	}
	cancel()
	select {
	case err := <-errc:
		if err != nil {
			t.Errorf("RunOnListener returned error: %v", err)
		}
	case <-time.After(12 * time.Second):
		t.Fatal("RunOnListener did not shut down")
	}
}

func TestRun(t *testing.T) {
	requireStack(t)
	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() { errc <- testSrv.Run(ctx) }()
	time.Sleep(300 * time.Millisecond) // let it bind and start serving
	cancel()
	select {
	case err := <-errc:
		if err != nil {
			t.Errorf("Run returned error: %v", err)
		}
	case <-time.After(12 * time.Second):
		t.Fatal("Run did not shut down")
	}
}

func TestServerVirtualUnreachableMembers(t *testing.T) {
	requireStack(t)
	// A virtual whose only member does not exist -> no members reachable.
	req(t, "POST", "/api/v2/virtuals", `{"name":"srv-vbad","package_type":"helm","members":["nonexistent-member"]}`)
	defer req(t, "DELETE", "/api/v2/virtuals/srv-vbad", "")
	if resp, _ := req(t, "GET", "/api/v1/virtual/srv-vbad/index.yaml", ""); resp.StatusCode != 502 {
		t.Errorf("virtual with dead members = %d, want 502", resp.StatusCode)
	}
}

func TestServerVirtualLocalPyPIMerge(t *testing.T) {
	requireStack(t)
	for _, n := range []string{"a", "b"} {
		req(t, "POST", "/api/v2/remotes", `{"name":"srv-pm-`+n+`","package_type":"pypi","repo_type":"local"}`)
		defer req(t, "DELETE", "/api/v2/remotes/srv-pm-"+n, "")
	}
	put(t, "/api/v2/remotes/srv-pm-a/files/foo-1.0-py3-none-any.whl", []byte("foo"))
	put(t, "/api/v2/remotes/srv-pm-b/files/bar-2.0-py3-none-any.whl", []byte("bar"))
	req(t, "POST", "/api/v2/virtuals", `{"name":"srv-pmv","package_type":"pypi","members":["srv-pm-a","srv-pm-b"]}`)
	defer req(t, "DELETE", "/api/v2/virtuals/srv-pmv", "")

	resp, b := req(t, "GET", "/api/v1/virtual/srv-pmv/simple/", "")
	if resp.StatusCode != 200 {
		t.Fatalf("virtual pypi index: %d %s", resp.StatusCode, b)
	}
	if s := string(b); !strings.Contains(s, "foo") || !strings.Contains(s, "bar") {
		t.Errorf("merged local pypi index missing packages: %s", s)
	}
}

func TestServerProxyErrors(t *testing.T) {
	requireStack(t)
	// Blocklisted path -> 403 propagated through handleProxy.
	block := fmt.Sprintf(`{"name":"srv-block","package_type":"generic","repo_type":"remote","base_url":%q,"blocklist":["\\.secret$"],"stale_on_error":true}`, upstream.URL)
	req(t, "POST", "/api/v2/remotes", block)
	defer req(t, "DELETE", "/api/v2/remotes/srv-block", "")
	if resp, _ := req(t, "GET", "/api/v1/remote/srv-block/x.secret", ""); resp.StatusCode != 403 {
		t.Errorf("blocklisted GET = %d, want 403", resp.StatusCode)
	}
	rq, _ := http.NewRequest("HEAD", testTS.URL+"/v2/srv-block/x.secret", nil)
	if resp, err := http.DefaultClient.Do(rq); err == nil {
		resp.Body.Close()
		if resp.StatusCode != 403 {
			t.Errorf("blocklisted HEAD = %d, want 403", resp.StatusCode)
		}
	}

	// Unreachable upstream, no stale copy -> 502 bad gateway.
	dead := `{"name":"srv-dead","package_type":"generic","repo_type":"remote","base_url":"http://127.0.0.1:1","stale_on_error":false}`
	req(t, "POST", "/api/v2/remotes", dead)
	defer req(t, "DELETE", "/api/v2/remotes/srv-dead", "")
	if resp, _ := req(t, "GET", "/api/v1/remote/srv-dead/x", ""); resp.StatusCode != 502 {
		t.Errorf("dead upstream GET = %d, want 502", resp.StatusCode)
	}
}

func TestServerLocalMissingBlob(t *testing.T) {
	requireStack(t)
	req(t, "POST", "/api/v2/remotes", `{"name":"srv-ghost","package_type":"generic","repo_type":"local"}`)
	defer req(t, "DELETE", "/api/v2/remotes/srv-ghost", "")

	ctx := context.Background()
	// A local file whose blob object is absent from the store.
	testSrv.db.UpsertBlob(ctx, "sha256:ghost", "blobs/sha256/ghost-missing", 5, "text/plain")
	if err := testSrv.db.CreateLocalFile(ctx, "srv-ghost", "ghost.bin", "sha256:ghost"); err != nil {
		t.Fatalf("create local file: %v", err)
	}

	if resp, _ := req(t, "GET", "/api/v1/local/srv-ghost/ghost.bin", ""); resp.StatusCode != 500 {
		t.Errorf("v1 download missing blob = %d, want 500", resp.StatusCode)
	}
	if resp, _ := req(t, "GET", "/api/v2/remotes/srv-ghost/files/ghost.bin", ""); resp.StatusCode != 500 {
		t.Errorf("v2 download missing blob = %d, want 500", resp.StatusCode)
	}
}

func TestServerBogusProviderType(t *testing.T) {
	requireStack(t)
	// Insert a remote with an unregistered package type directly, bypassing
	// validation, to exercise the provider-not-found branches.
	_, err := testSrv.db.Pool.Exec(context.Background(),
		`INSERT INTO remotes (name, package_type, repo_type, base_url) VALUES ($1,'bogus','remote','https://x')`, "srv-bogus")
	if err != nil {
		t.Fatalf("insert bogus remote: %v", err)
	}
	defer testSrv.db.Pool.Exec(context.Background(), `DELETE FROM remotes WHERE name='srv-bogus'`)

	if resp, _ := req(t, "GET", "/api/v1/remote/srv-bogus/x", ""); resp.StatusCode != 500 {
		t.Errorf("bogus provider GET = %d, want 500", resp.StatusCode)
	}
	rq, _ := http.NewRequest("HEAD", testTS.URL+"/v2/srv-bogus/x", nil)
	if resp, err := http.DefaultClient.Do(rq); err == nil {
		resp.Body.Close()
		if resp.StatusCode != 500 {
			t.Errorf("bogus provider HEAD = %d, want 500", resp.StatusCode)
		}
	}
	if resp, b := req(t, "POST", "/api/v2/probe", `{"remote":"srv-bogus","path":"x"}`); resp.StatusCode != 200 || !strings.Contains(string(b), `"status":500`) {
		t.Errorf("bogus provider probe: %d %s", resp.StatusCode, b)
	}
}

func TestServerNotFound(t *testing.T) {
	requireStack(t)
	if resp, _ := req(t, "GET", "/api/v2/remotes/does-not-exist", ""); resp.StatusCode != 404 {
		t.Errorf("expected 404, got %d", resp.StatusCode)
	}
	if resp, _ := req(t, "GET", "/api/v1/remote/nope/x", ""); resp.StatusCode != 404 {
		t.Errorf("expected 404 for unknown remote, got %d", resp.StatusCode)
	}
	// Unknown local repo -> 404 in handleLocal.
	if resp, _ := req(t, "GET", "/api/v1/local/nope/x", ""); resp.StatusCode != 404 {
		t.Errorf("expected 404 for unknown local repo, got %d", resp.StatusCode)
	}
}
