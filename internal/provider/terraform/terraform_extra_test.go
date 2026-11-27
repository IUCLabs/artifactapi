package terraform

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/IUCLabs/artifactapi/internal/provider"
	"github.com/IUCLabs/artifactapi/pkg/models"
)

type fakeFileStore struct{ entries []provider.FileEntry }

func (f fakeFileStore) ListFilesByPrefix(_ context.Context, _, prefix string) ([]provider.FileEntry, error) {
	var out []provider.FileEntry
	for _, e := range f.entries {
		if strings.HasPrefix(e.FilePath, prefix) {
			out = append(out, e)
		}
	}
	return out, nil
}
func (f fakeFileStore) ListPackages(_ context.Context, _ string) ([]string, error) { return nil, nil }

func TestTFPureFuncs(t *testing.T) {
	p := &Provider{}
	if p.Classify("hashicorp/aws/versions") != provider.Mutable {
		t.Error("versions should be mutable")
	}
	if p.Classify("hashicorp/aws/terraform-provider-aws_1.0.0_linux_amd64.zip") != provider.Immutable {
		t.Error("zip should be immutable")
	}
	if got := p.UpstreamURL(models.Remote{BaseURL: "https://registry.terraform.io"}, "hashicorp/aws/versions"); got != "https://registry.terraform.io/v1/providers/hashicorp/aws/versions" {
		t.Errorf("upstream url %q", got)
	}
	h, _ := p.AuthHeaders(context.Background(), models.Remote{Username: "u", Password: "p"})
	if h.Get("Authorization") == "" {
		t.Error("auth header")
	}
	_ = p.ContentType("x.json")
}

func TestTFValidateUpload(t *testing.T) {
	p := &Provider{}
	sp, ct, err := p.ValidateUpload("hashicorp/aws/terraform-provider-aws_1.2.3_linux_amd64.zip")
	if err != nil || sp != "hashicorp/aws/terraform-provider-aws_1.2.3_linux_amd64.zip" || ct != "application/zip" {
		t.Errorf("valid: sp=%q ct=%q err=%v", sp, ct, err)
	}
	if _, _, err := p.ValidateUpload("too/few"); err == nil {
		t.Error("expected error for wrong path depth")
	}
	if _, _, err := p.ValidateUpload("ns/aws/not-a-provider.zip"); err == nil {
		t.Error("expected error for bad filename")
	}
	if _, _, err := p.ValidateUpload("ns/gcp/terraform-provider-aws_1.0.0_linux_amd64.zip"); err == nil {
		t.Error("expected error for type mismatch")
	}
}

func TestTFUploadResponse(t *testing.T) {
	p := &Provider{}
	resp := p.UploadResponse("hashicorp/aws/terraform-provider-aws_1.2.3_linux_amd64.zip", "sha256:abc", 100)
	if resp["namespace"] != "hashicorp" || resp["type"] != "aws" || resp["version"] != "1.2.3" || resp["os"] != "linux" || resp["arch"] != "amd64" {
		t.Errorf("structured response wrong: %v", resp)
	}
	fallback := p.UploadResponse("weird/path", "sha256:x", 1)
	if fallback["path"] != "weird/path" {
		t.Errorf("fallback response wrong: %v", fallback)
	}
}

func TestTFRewriteServedNoop(t *testing.T) {
	p := &Provider{}
	remote := models.Remote{Name: "tf", ReleasesRemote: "hashicorp-releases"}
	for _, tc := range []struct {
		name   string
		remote models.Remote
		body   string
	}{
		{"no releases remote", models.Remote{Name: "tf"}, `{"download_url":"https://releases.hashicorp.com/a.zip"}`},
		{"not json", remote, "not json"},
		{"versions document", remote, `{"versions":[{"version":"1.0.0"}]}`},
		{"relative url", remote, `{"download_url":"a.zip"}`},
		{"unparseable url", remote, `{"download_url":"://bad"}`},
	} {
		if got := string(p.RewriteServed([]byte(tc.body), tc.remote, "http://proxy")); got != tc.body {
			t.Errorf("%s: body changed to %s", tc.name, got)
		}
	}
}

func TestTFServeLocalIndex(t *testing.T) {
	p := &Provider{}
	fs := fakeFileStore{entries: []provider.FileEntry{
		{FilePath: "hashicorp/aws/terraform-provider-aws_1.0.0_linux_amd64.zip", ContentHash: "sha256:deadbeef"},
		{FilePath: "hashicorp/aws/terraform-provider-aws_1.0.0_darwin_arm64.zip", ContentHash: "sha256:cafe"},
	}}

	serve := func(path string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodGet, "/"+path, nil)
		p.ServeLocalIndex(w, r, fs, "repo", path)
		return w
	}

	if w := serve("hashicorp/aws/index.json"); w.Code != 200 || !strings.Contains(w.Body.String(), "1.0.0") {
		t.Errorf("index.json: code=%d body=%s", w.Code, w.Body.String())
	}
	if w := serve("hashicorp/aws/1.0.0.json"); w.Code != 200 || !strings.Contains(w.Body.String(), "linux_amd64") {
		t.Errorf("version doc: code=%d body=%s", w.Code, w.Body.String())
	}

	// Not a terraform index path.
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/x", nil)
	if p.ServeLocalIndex(w, r, fs, "repo", "hashicorp/aws/other.txt") {
		t.Error("non-index path should return false")
	}
	if p.ServeLocalIndex(httptest.NewRecorder(), r, fs, "repo", "too/short") {
		t.Error("short path should return false")
	}
}

func TestTFContentTypeAndEmptyIndex(t *testing.T) {
	p := &Provider{}
	for path, want := range map[string]string{
		"x.zip":      "application/zip",
		"x.sig":      "application/octet-stream",
		"index.json": "application/json",
	} {
		if got := p.ContentType(path); got != want {
			t.Errorf("ContentType(%q)=%q want %q", path, got, want)
		}
	}
	// index / version doc with no matching files -> 404.
	empty := fakeFileStore{}
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/hashicorp/aws/index.json", nil)
	p.ServeLocalIndex(w, r, empty, "repo", "hashicorp/aws/index.json")
	if w.Code != http.StatusNotFound {
		t.Errorf("empty index should be 404, got %d", w.Code)
	}
	w = httptest.NewRecorder()
	p.ServeLocalIndex(w, r, empty, "repo", "hashicorp/aws/1.0.0.json")
	if w.Code != http.StatusNotFound {
		t.Errorf("empty version doc should be 404, got %d", w.Code)
	}
}

func TestTFGenerateLocalIndexUnsupported(t *testing.T) {
	if _, err := (&Provider{}).GenerateLocalIndex(context.Background(), fakeFileStore{}, "r", "x"); err == nil {
		t.Error("expected unsupported error")
	}
}
