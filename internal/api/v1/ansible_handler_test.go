package v1

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"strings"
	"testing"

	"github.com/IUCLabs/artifactapi/internal/virtual"
	"github.com/IUCLabs/artifactapi/pkg/models"
)

func testCollection(t *testing.T, ns, name, version string) []byte {
	t.Helper()
	manifest := `{"collection_info":{"namespace":"` + ns + `","name":"` + name + `","version":"` + version + `","dependencies":{"ansible.posix":">=1.0.0"}}}`
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	_ = tw.WriteHeader(&tar.Header{Name: "MANIFEST.json", Mode: 0o644, Size: int64(len(manifest))})
	_, _ = tw.Write([]byte(manifest))
	_ = tw.Close()
	_ = zw.Close()
	return buf.Bytes()
}

// ansiblePublish uploads like ansible-galaxy: the file part base64-encoded
// whenever a sha256 field is sent, raw otherwise.
func ansiblePublish(t *testing.T, url string, tarball []byte, sum string) (int, string) {
	t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	hdr := textproto.MIMEHeader{"Content-Disposition": {`form-data; name="file"; filename="c.tar.gz"`}}
	data := tarball
	if sum != "" {
		_ = mw.WriteField("sha256", sum)
		hdr.Set("Content-Transfer-Encoding", "base64")
		data = []byte(base64.StdEncoding.EncodeToString(tarball))
	}
	fw, _ := mw.CreatePart(hdr)
	_, _ = fw.Write(data)
	_ = mw.Close()
	req, _ := http.NewRequest(http.MethodPost, url, &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var b bytes.Buffer
	_, _ = b.ReadFrom(resp.Body)
	return resp.StatusCode, b.String()
}

func getJSON(t *testing.T, url string, v any) {
	t.Helper()
	code, body := npmDo(t, http.MethodGet, url, "")
	if code != http.StatusOK {
		t.Fatalf("GET %s = %d: %s", url, code, body)
	}
	if err := json.Unmarshal([]byte(body), v); err != nil {
		t.Fatalf("GET %s: %v: %s", url, err, body)
	}
}

func TestAnsiblePublishAndServe(t *testing.T) {
	h, srv := npmTestServer(t)
	repo := npmRepo(t, h, models.PackageAnsible)
	base := srv.URL + "/api/v1/local/" + repo + "/"
	publish := base + "api/v3/artifacts/collections/"

	v1 := testCollection(t, "myorg", "e2e_hello", "1.0.0")
	code, body := ansiblePublish(t, publish, v1, sha256Hex(v1))
	if code != http.StatusAccepted {
		t.Fatalf("publish = %d: %s", code, body)
	}
	var task struct{ Task string }
	_ = json.Unmarshal([]byte(body), &task)
	if task.Task != base+"api/v3/imports/collections/myorg-e2e_hello-1.0.0/" {
		t.Fatalf("task = %q", task.Task)
	}
	var st map[string]any
	getJSON(t, task.Task, &st)
	if st["state"] != "completed" || st["finished_at"] == "" {
		t.Fatalf("task = %v", st)
	}
	if code, _ := npmDo(t, http.MethodGet, base+"api/v3/imports/collections/myorg-e2e_hello-9.9.9/", ""); code != http.StatusNotFound {
		t.Errorf("unknown task = %d, want 404", code)
	}

	for _, v := range []string{"1.1.0", "2.0.0"} {
		tb := testCollection(t, "myorg", "e2e_hello", v)
		if code, body := ansiblePublish(t, publish, tb, ""); code != http.StatusAccepted {
			t.Fatalf("publish %s = %d: %s", v, code, body)
		}
	}
	if code, body := ansiblePublish(t, publish, v1, ""); code != http.StatusConflict || !strings.Contains(body, "already exists") {
		t.Errorf("identical republish = %d: %s, want 409", code, body)
	}
	if code, _ := ansiblePublish(t, publish, testCollection(t, "myorg", "e2e_hello", "1.0.0")[:20], ""); code != http.StatusBadRequest {
		t.Errorf("truncated tarball = %d, want 400", code)
	}

	var root map[string]map[string]string
	getJSON(t, base+"api/", &root)
	if root["available_versions"]["v3"] != "v3/" {
		t.Fatalf("root = %v", root)
	}

	var page struct {
		Meta  struct{ Count int }
		Links struct{ Next, Previous *string }
		Data  []struct{ Version, Href string }
	}
	getJSON(t, base+"api/v3/collections/myorg/e2e_hello/versions/?limit=2", &page)
	if page.Meta.Count != 3 || len(page.Data) != 2 || page.Data[0].Version != "2.0.0" || page.Links.Next == nil || page.Links.Previous != nil {
		t.Fatalf("page 1 = %+v", page)
	}
	next := *page.Links.Next
	if !strings.HasPrefix(next, "/api/v1/local/"+repo+"/") {
		t.Fatalf("next = %q, want root-relative", next)
	}
	next = srv.URL + next
	page.Links.Next = nil
	getJSON(t, next, &page)
	if len(page.Data) != 1 || page.Data[0].Version != "1.0.0" || page.Links.Next != nil {
		t.Fatalf("page 2 (%s) = %+v", next, page)
	}

	var detail struct {
		DownloadURL string `json:"download_url"`
		Artifact    struct{ Sha256 string }
		Metadata    struct{ Dependencies map[string]string }
		Namespace   struct{ Name string }
		Collection  struct{ Name string }
	}
	getJSON(t, page.Data[0].Href, &detail)
	if detail.DownloadURL != base+"artifacts/myorg-e2e_hello-1.0.0.tar.gz" || detail.Artifact.Sha256 != sha256Hex(v1) ||
		detail.Metadata.Dependencies["ansible.posix"] != ">=1.0.0" || detail.Namespace.Name != "myorg" || detail.Collection.Name != "e2e_hello" {
		t.Fatalf("detail = %+v", detail)
	}
	if code, got := npmDo(t, http.MethodGet, detail.DownloadURL, ""); code != http.StatusOK || got != string(v1) {
		t.Fatalf("download = %d (%d bytes)", code, len(got))
	}

	var coll map[string]any
	getJSON(t, base+"api/v3/collections/myorg/e2e_hello/", &coll)
	if coll["versions_url"] != base+"api/v3/collections/myorg/e2e_hello/versions/" || coll["updated_at"] == "" {
		t.Fatalf("collection = %v", coll)
	}
	for _, p := range []string{"api/v3/collections/myorg/nope/", "api/v3/collections/myorg/e2e_hello/versions/9.0.0/", "_index/myorg/e2e_hello/1.0.0", "other"} {
		if code, _ := npmDo(t, http.MethodGet, base+p, ""); code != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404", p, code)
		}
	}
}

func TestAnsiblePublishRejects(t *testing.T) {
	h, srv := npmTestServer(t)
	repo := npmRepo(t, h, models.PackageAnsible)
	publish := srv.URL + "/api/v1/local/" + repo + "/api/v3/artifacts/collections/"
	tb := testCollection(t, "myorg", "rej", "1.0.0")

	if code, _ := ansiblePublish(t, publish, tb, strings.Repeat("0", 64)); code != http.StatusBadRequest {
		t.Errorf("sha256 mismatch = %d, want 400", code)
	}
	if code, _ := npmDo(t, http.MethodPost, publish, "raw"); code != http.StatusBadRequest {
		t.Errorf("non-multipart = %d, want 400", code)
	}
	old := ansibleMaxPublishBytes
	ansibleMaxPublishBytes = 64
	if code, _ := ansiblePublish(t, publish, tb, ""); code != http.StatusRequestEntityTooLarge {
		t.Errorf("oversize = %d, want 413", code)
	}
	ansibleMaxPublishBytes = old
	generic := npmRepo(t, h, models.PackageGeneric)
	if code, _ := ansiblePublish(t, srv.URL+"/api/v1/local/"+generic+"/api/v3/artifacts/collections/", tb, ""); code != http.StatusMethodNotAllowed {
		t.Errorf("publish to generic local = %d, want 405", code)
	}
	if code, _ := ansiblePublish(t, publish, tb, ""); code != http.StatusAccepted {
		t.Errorf("publish after rejects = %d, want 202", code)
	}
}

func TestAnsibleLocalPerHost(t *testing.T) {
	h, srv := npmTestServer(t)
	repo := npmRepo(t, h, models.PackageAnsible)
	if code, b := ansiblePublish(t, srv.URL+"/api/v1/local/"+repo+"/api/v3/artifacts/collections/", testCollection(t, "myorg", "host", "1.0.0"), ""); code != http.StatusAccepted {
		t.Fatalf("publish = %d: %s", code, b)
	}
	routes := h.Routes()
	path := "/local/" + repo + "/api/v3/collections/myorg/host/versions/1.0.0/"
	r := httptest.NewRequest(http.MethodGet, path, nil)
	r.Host = "x@evil.com"
	w := httptest.NewRecorder()
	routes.ServeHTTP(w, r)
	if w.Code != http.StatusBadRequest {
		t.Errorf("bad Host = %d, want 400", w.Code)
	}
	r = httptest.NewRequest(http.MethodGet, path, nil)
	r.Host = "aa.example"
	r.Header.Set("X-Forwarded-Proto", "https")
	w = httptest.NewRecorder()
	routes.ServeHTTP(w, r)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"download_url":"https://aa.example/api/v1/local/`+repo+`/artifacts/myorg-host-1.0.0.tar.gz"`) {
		t.Fatalf("detail = %d: %s", w.Code, w.Body)
	}
	if cc, vary := w.Header().Get("Cache-Control"), w.Header().Get("Vary"); cc != "no-store" || vary != "Host, X-Forwarded-Proto" {
		t.Errorf("Cache-Control %q Vary %q", cc, vary)
	}
}

func TestAnsibleVirtualOverLocals(t *testing.T) {
	h, srv := npmTestServer(t)
	h.virtualEngine = virtual.NewEngine(h.db, nil)
	h.virtualEngine.AnsibleLocal = h.LocalAnsible
	a, b := npmRepo(t, h, models.PackageAnsible), npmRepo(t, h, models.PackageAnsible)
	for _, p := range []struct{ repo, version string }{{a, "1.0.0"}, {b, "1.0.0"}, {b, "2.0.0"}} {
		if code, body := ansiblePublish(t, srv.URL+"/api/v1/local/"+p.repo+"/api/v3/artifacts/collections/", testCollection(t, "myorg", "virt", p.version), ""); code != http.StatusAccepted {
			t.Fatalf("publish %s to %s = %d: %s", p.version, p.repo, code, body)
		}
	}
	virt := "v" + a
	if err := h.db.CreateVirtual(context.Background(), &models.Virtual{Name: virt, PackageType: models.PackageAnsible, Members: []string{a, b}}); err != nil {
		t.Fatal(err)
	}
	base := srv.URL + "/api/v1/virtual/" + virt + "/"

	var page struct {
		Meta  struct{ Count int }
		Links struct{ Next *string }
		Data  []struct{ Version, Href string }
	}
	getJSON(t, base+"api/v3/collections/myorg/virt/versions/?limit=1", &page)
	if page.Meta.Count != 2 || len(page.Data) != 1 || page.Data[0].Version != "1.0.0" || page.Links.Next == nil {
		t.Fatalf("page 1 = %+v", page)
	}
	href := page.Data[0].Href
	if !strings.HasPrefix(*page.Links.Next, "/api/v1/virtual/"+virt+"/") {
		t.Fatalf("next = %q, want root-relative", *page.Links.Next)
	}
	getJSON(t, srv.URL+*page.Links.Next, &page)
	if len(page.Data) != 1 || page.Data[0].Version != "2.0.0" || page.Links.Next != nil {
		t.Fatalf("page 2 = %+v", page)
	}

	var detail struct {
		DownloadURL string `json:"download_url"`
	}
	getJSON(t, href, &detail)
	if detail.DownloadURL != base+a+"/artifacts/myorg-virt-1.0.0.tar.gz" {
		t.Fatalf("download_url = %q, want the first member via the virtual", detail.DownloadURL)
	}
	noFollow := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := noFollow.Get(detail.DownloadURL)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusFound || resp.Header.Get("Location") != srv.URL+"/api/v1/local/"+a+"/artifacts/myorg-virt-1.0.0.tar.gz" {
		t.Fatalf("download = %d -> %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	for _, p := range []string{"api/v3/collections/myorg/nope/", "notamember/x.tar.gz"} {
		if code, _ := npmDo(t, http.MethodGet, base+p, ""); code != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404", p, code)
		}
	}
}
