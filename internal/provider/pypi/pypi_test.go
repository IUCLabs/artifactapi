package pypi

import (
	"context"
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

func TestRewriteResponse(t *testing.T) {
	p := &Provider{}
	if out, _ := p.RewriteResponse([]byte("x"), models.Remote{Name: "pypi"}, ""); out != nil {
		t.Error("empty proxyBaseURL is a no-op")
	}
	body := []byte(`<a href="https://files.pythonhosted.org/packages/foo.whl">foo.whl</a>`)
	out, err := p.RewriteResponse(body, models.Remote{Name: "pypi"}, "http://proxy")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "http://proxy/api/v1/remote/pypi/") {
		t.Errorf("not rewritten: %s", out)
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
