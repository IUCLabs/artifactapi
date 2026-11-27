package pypi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/IUCLabs/artifactapi/internal/provider"
	"github.com/IUCLabs/artifactapi/pkg/models"
)

// fakeFileStore is an in-memory provider.FileStore for exercising local index
// generation without a database.
type fakeFileStore struct {
	packages []string
	files    map[string][]provider.FileEntry
}

func (f *fakeFileStore) ListPackages(_ context.Context, _ string) ([]string, error) {
	return f.packages, nil
}

func (f *fakeFileStore) ListFilesByPrefix(_ context.Context, _, prefix string) ([]provider.FileEntry, error) {
	return f.files[prefix], nil
}

func TestTypeClassifyContentType(t *testing.T) {
	p := &Provider{}
	if p.Type() != models.PackagePyPI {
		t.Fatal("type")
	}
	if p.Classify("simple/foo/") != provider.Mutable {
		t.Error("simple index should be mutable")
	}
	if p.Classify("packages/foo-1.0.whl") != provider.Immutable {
		t.Error("wheel should be immutable")
	}
	cases := map[string]string{
		"foo-1.0-py3-none-any.whl": "application/zip",
		"foo-1.0.zip":              "application/zip",
		"foo-1.0.tar.gz":           "application/gzip",
		"simple/foo/":              "text/html",
		"weird":                    "application/octet-stream",
	}
	for path, want := range cases {
		if got := p.ContentType(path); got != want {
			t.Errorf("ContentType(%q)=%q want %q", path, got, want)
		}
	}
}

func TestUpstreamURL(t *testing.T) {
	p := &Provider{}
	if got := p.UpstreamURL(models.Remote{BaseURL: "https://files.example.com"}, "packages/foo.whl"); got != "https://files.example.com/packages/foo.whl" {
		t.Errorf("got %q", got)
	}
	if got := p.UpstreamURL(models.Remote{BaseURL: "https://x"}, "simple/foo/"); got != "https://pypi.org/simple/foo/" {
		t.Errorf("simple should hit pypi.org, got %q", got)
	}
}

func TestValidateUpload(t *testing.T) {
	p := &Provider{}
	sp, ct, err := p.ValidateUpload("numpy-1.26.0-cp311-cp311-linux_x86_64.whl")
	if err != nil || sp != "numpy/numpy-1.26.0-cp311-cp311-linux_x86_64.whl" || ct != "application/zip" {
		t.Errorf("wheel: sp=%q ct=%q err=%v", sp, ct, err)
	}
	sp, ct, err = p.ValidateUpload("requests-2.31.0.tar.gz")
	if err != nil || sp != "requests/requests-2.31.0.tar.gz" || ct != "application/gzip" {
		t.Errorf("sdist: sp=%q ct=%q err=%v", sp, ct, err)
	}
	if _, _, err := p.ValidateUpload("not-a-package.txt"); err == nil {
		t.Error("expected error for bad extension")
	}
}

func TestPackageNameParsing(t *testing.T) {
	if got := packageFromWheel("Foo_Bar-1.0-py3-none-any.whl"); got != "foo-bar" {
		t.Errorf("wheel name = %q", got)
	}
	if got := packageFromWheel("noseparator.whl"); got != "" {
		t.Errorf("expected empty for unparseable wheel, got %q", got)
	}
	if got := packageFromSdist("My.Pkg-2.0.tar.gz"); got != "my-pkg" {
		t.Errorf("sdist name = %q", got)
	}
	if got := packageFromSdist("noseparator.zip"); got != "" {
		t.Errorf("expected empty, got %q", got)
	}
}

func TestUploadResponse(t *testing.T) {
	resp := (&Provider{}).UploadResponse("foo/foo-1.0.whl", "sha256:abc", 123)
	if resp["filename"] != "foo-1.0.whl" || resp["package"] != "foo" || resp["content_hash"] != "sha256:abc" {
		t.Errorf("unexpected upload response: %v", resp)
	}
}

const (
	wheelPath = "packages/b7/ce/149a/six-1.17.0-py2.py3-none-any.whl"
	wheelHash = "4721f391ed90541fddacab5acf947aa0d3dc7d27b2e1e8eda2be8970586c3274"
)

var pypiRemote = models.Remote{Name: "pypi", BaseURL: "https://pypi.org"}

func TestRewriteResponseIsNoop(t *testing.T) {
	if out, err := (&Provider{}).RewriteResponse([]byte("x"), pypiRemote, ""); out != nil || err != nil {
		t.Errorf("cached bodies must stay upstream-shaped, got %q, %v", out, err)
	}
	if _, ok := any(&Provider{}).(provider.ResponseRewriter); ok {
		t.Error("pypi rewrites at serve time, not before caching")
	}
}

// proxied asserts url points at the remote and that its path routes back
// through UpstreamURL to the original file as an immutable (cached) artifact.
func proxied(t *testing.T, url string) {
	t.Helper()
	const prefix = "https://aa.example:8443/api/v1/remote/pypi/"
	if !strings.HasPrefix(url, prefix) {
		t.Fatalf("%q not rewritten to the remote", url)
	}
	path, _, _ := strings.Cut(strings.TrimPrefix(url, prefix), "#")
	p := &Provider{}
	if p.Classify(path) != provider.Immutable {
		t.Errorf("%q must be cached as immutable", path)
	}
	if got := p.UpstreamURL(pypiRemote, path); got != "https://pypi.org/"+path {
		t.Errorf("UpstreamURL(%q) = %q", path, got)
	}
}

func TestRewriteServedSimpleHTML(t *testing.T) {
	body := `<a href="https://files.pythonhosted.org/` + wheelPath + `#sha256=` + wheelHash +
		`" data-requires-python="&gt;=2.7" data-dist-info-metadata="sha256=abc" data-core-metadata="sha256=abc">six-1.17.0-py2.py3-none-any.whl</a>`
	out := string((&Provider{}).RewriteServed([]byte(body), pypiRemote, "https://aa.example:8443/"))
	want := `<a href="https://aa.example:8443/api/v1/remote/pypi/` + wheelPath + `#sha256=` + wheelHash +
		`" data-requires-python="&gt;=2.7" data-dist-info-metadata="sha256=abc" data-core-metadata="sha256=abc">six-1.17.0-py2.py3-none-any.whl</a>`
	if out != want {
		t.Fatalf("got  %s\nwant %s", out, want)
	}
	href, _, _ := strings.Cut(strings.TrimPrefix(out, `<a href="`), `"`)
	proxied(t, href)
	metadata, _, _ := strings.Cut(href, "#")
	proxied(t, metadata+".metadata")
}

func TestRewriteServedProjectList(t *testing.T) {
	out := string((&Provider{}).RewriteServed([]byte(`<a href="/simple/six/">six</a>`), pypiRemote, "https://aa.example:8443"))
	if out != `<a href="https://aa.example:8443/api/v1/remote/pypi/simple/six/">six</a>` {
		t.Errorf("root-relative project link not rewritten: %s", out)
	}
}

func TestRewriteServedJSON(t *testing.T) {
	cases := map[string]string{
		"simple PEP 691": `{"meta":{"api-version":"1.1"},"name":"six","files":[{"filename":"six.whl","url":"https://files.pythonhosted.org/` + wheelPath + `","hashes":{"sha256":"` + wheelHash + `"},"core-metadata":{"sha256":"abc"},"data-dist-info-metadata":{"sha256":"abc"}}]}`,
		"JSON API":       `{"info":{"name":"six","project_url":"https://pypi.org/project/six/"},"urls":[{"filename":"six.whl","url":"https://files.pythonhosted.org/` + wheelPath + `","digests":{"sha256":"` + wheelHash + `"}}]}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			out := (&Provider{}).RewriteServed([]byte(body), pypiRemote, "https://aa.example:8443")
			var doc struct {
				Info  map[string]string `json:"info"`
				Files []struct {
					URL          string            `json:"url"`
					Hashes       map[string]string `json:"hashes"`
					Digests      map[string]string `json:"digests"`
					CoreMetadata map[string]string `json:"core-metadata"`
				} `json:"files"`
				URLs []struct {
					URL     string            `json:"url"`
					Digests map[string]string `json:"digests"`
				} `json:"urls"`
			}
			if err := json.Unmarshal(out, &doc); err != nil {
				t.Fatalf("rewritten body is not JSON: %v\n%s", err, out)
			}
			for _, f := range doc.Files {
				proxied(t, f.URL)
				if f.Hashes["sha256"] != wheelHash || f.CoreMetadata["sha256"] != "abc" {
					t.Errorf("hashes/metadata lost: %+v", f)
				}
			}
			for _, u := range doc.URLs {
				proxied(t, u.URL)
				if u.Digests["sha256"] != wheelHash {
					t.Errorf("digest lost: %+v", u)
				}
			}
			if len(doc.Files)+len(doc.URLs) != 1 {
				t.Fatalf("file entries missing: %s", out)
			}
			if got := doc.Info["project_url"]; got != "" && got != "https://pypi.org/project/six/" {
				t.Errorf("non-file URL rewritten: %q", got)
			}
		})
	}
}

func TestJSONAPIPaths(t *testing.T) {
	p := &Provider{}
	for _, path := range []string{"pypi/six/json", "pypi/six/1.17.0/json", "pypi/six/json/"} {
		if p.Classify(path) != provider.Mutable {
			t.Errorf("%q should be mutable", path)
		}
		if got := p.ContentType(path); got != "application/json" {
			t.Errorf("ContentType(%q) = %q", path, got)
		}
		if got := p.UpstreamURL(models.Remote{BaseURL: "https://files.example.com"}, path); got != "https://pypi.org/"+path {
			t.Errorf("UpstreamURL(%q) = %q", path, got)
		}
	}
	if p.Classify("packages/aa/pypi/x/json") != provider.Immutable {
		t.Error("only top-level pypi/<pkg>/json is the API")
	}
}

func TestGenerateLocalIndex(t *testing.T) {
	p := &Provider{}
	fs := &fakeFileStore{
		packages: []string{"foo", "bar"},
		files: map[string][]provider.FileEntry{
			"foo/": {{FilePath: "foo/foo-1.0-py3-none-any.whl", ContentHash: "sha256:aaa"}},
		},
	}
	list, err := p.GenerateLocalIndex(context.Background(), fs, "local", "simple/")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(list), "foo") || !strings.Contains(string(list), "bar") {
		t.Errorf("package list missing entries: %s", list)
	}

	files, err := p.GenerateLocalIndex(context.Background(), fs, "local", "simple/foo/")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(files), "foo-1.0-py3-none-any.whl") {
		t.Errorf("file list missing wheel: %s", files)
	}

	if _, err := p.GenerateLocalIndex(context.Background(), fs, "local", "notsimple"); err == nil {
		t.Error("expected error for non-simple path")
	}
}

func TestServeLocalIndexHTTP(t *testing.T) {
	p := &Provider{}
	fs := &fakeFileStore{
		packages: []string{"foo"},
		files: map[string][]provider.FileEntry{
			"foo/": {{FilePath: "foo/foo-1.0-py3-none-any.whl", ContentHash: "sha256:aaa"}},
		},
	}
	serve := func(path string) (*httptest.ResponseRecorder, bool) {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodGet, "/"+path, nil)
		handled := p.ServeLocalIndex(w, r, fs, "local", path)
		return w, handled
	}

	if w, ok := serve("simple/"); !ok || w.Code != 200 || !strings.Contains(w.Body.String(), "foo") {
		t.Errorf("simple index: handled=%v code=%d body=%s", ok, w.Code, w.Body.String())
	}
	if w, ok := serve("simple/foo/"); !ok || w.Code != 200 || !strings.Contains(w.Body.String(), "foo-1.0-py3-none-any.whl") {
		t.Errorf("package index: handled=%v code=%d body=%s", ok, w.Code, w.Body.String())
	}
	// Non-simple paths are not handled.
	if _, ok := serve("packages/foo.whl"); ok {
		t.Error("non-index path should not be handled")
	}
}

func TestAuthHeaders(t *testing.T) {
	h, _ := (&Provider{}).AuthHeaders(context.Background(), models.Remote{Username: "u", Password: "p"})
	if h.Get("Authorization") == "" {
		t.Error("expected auth header")
	}
}
