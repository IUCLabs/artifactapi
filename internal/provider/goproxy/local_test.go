package goproxy

import (
	"archive/zip"
	"context"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/mod/module"
	modzip "golang.org/x/mod/zip"

	"github.com/IUCLabs/artifactapi/internal/provider"
)

type fakeFileStore struct{ paths []string }

func (f *fakeFileStore) ListPackages(context.Context, string) ([]string, error) { return nil, nil }

func (f *fakeFileStore) ListFilesByPrefix(_ context.Context, _, prefix string) ([]provider.FileEntry, error) {
	var out []provider.FileEntry
	for _, p := range f.paths {
		if strings.HasPrefix(p, prefix) {
			out = append(out, provider.FileEntry{FilePath: p, ContentHash: "sha256:x"})
		}
	}
	return out, nil
}

func TestValidateUpload(t *testing.T) {
	p := &Provider{}
	cases := []struct {
		in, want string
		ok       bool
	}{
		{"myorg.example.net/hello/@v/v0.1.0.zip", "myorg.example.net/hello/@v/v0.1.0.zip", true},
		{"/myorg.example.net/hello/@v/v0.1.0.mod", "myorg.example.net/hello/@v/v0.1.0.mod", true},
		{"github.com/!burnt!sushi/toml/@v/v1.2.0.info", "github.com/!burnt!sushi/toml/@v/v1.2.0.info", true},
		{"myorg.example.net/hello/v2/@v/v2.0.0-rc.1.zip", "myorg.example.net/hello/v2/@v/v2.0.0-rc.1.zip", true},
		{"github.com/BurntSushi/toml/@v/v1.2.0.zip", "", false},
		{"myorg.example.net/hello/@v/v0.1.zip", "", false},
		{"myorg.example.net/hello/@v/v2.0.0.zip", "", false},
		{"myorg.example.net/hello/@v/v0.1.0.tar.gz", "", false},
		{"myorg.example.net/hello/v0.1.0.zip", "", false},
		{"myorg.example.net/hello/@v/list", "", false},
	}
	for _, c := range cases {
		got, ct, err := p.ValidateUpload(c.in)
		if (err == nil) != c.ok {
			t.Errorf("%s: err=%v, want ok=%v", c.in, err, c.ok)
			continue
		}
		if c.ok && (got != c.want || ct == "") {
			t.Errorf("%s: got %q %q, want %q", c.in, got, ct, c.want)
		}
	}
}

func writeZip(t *testing.T, files map[string]string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "m.zip")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(f)
	for name, body := range files {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

func writeFile(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "f")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestValidateContentZip(t *testing.T) {
	p := &Provider{}
	sp := "myorg.example.net/hello/@v/v0.1.0.zip"

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module myorg.example.net/hello\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	good := filepath.Join(t.TempDir(), "good.zip")
	f, _ := os.Create(good)
	if err := modzip.CreateFromDir(f, module.Version{Path: "myorg.example.net/hello", Version: "v0.1.0"}, dir); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	if err := p.ValidateContent(sp, good); err != nil {
		t.Errorf("valid zip rejected: %v", err)
	}

	bad := map[string]map[string]string{
		"no prefix":     {"go.mod": "module myorg.example.net/hello\n"},
		"wrong version": {"myorg.example.net/hello@v0.2.0/go.mod": "module myorg.example.net/hello\n"},
		"wrong module":  {"myorg.example.net/other@v0.1.0/go.mod": "module myorg.example.net/other\n"},
	}
	for name, files := range bad {
		if err := p.ValidateContent(sp, writeZip(t, files)); err == nil {
			t.Errorf("%s: expected rejection", name)
		}
	}
	if err := p.ValidateContent(sp, writeFile(t, "not a zip")); err == nil {
		t.Error("non-zip accepted")
	}
}

func TestValidateContentModInfo(t *testing.T) {
	p := &Provider{}
	mod := "myorg.example.net/hello/@v/v0.1.0.mod"
	info := "myorg.example.net/hello/@v/v0.1.0.info"
	cases := []struct {
		path, body string
		ok         bool
	}{
		{mod, "module myorg.example.net/hello\n\ngo 1.25\n", true},
		{mod, "module myorg.example.net/other\n", false},
		{mod, "go 1.25\n", false},
		{mod, "module (\n", false},
		{info, `{"Version":"v0.1.0","Time":"2026-01-01T00:00:00Z"}`, true},
		{info, `{"Version":"v0.2.0"}`, false},
		{info, `nope`, false},
	}
	for _, c := range cases {
		if err := p.ValidateContent(c.path, writeFile(t, c.body)); (err == nil) != c.ok {
			t.Errorf("%s %q: err=%v, want ok=%v", c.path, c.body, err, c.ok)
		}
	}
}

func serve(t *testing.T, fs provider.FileStore, path string) (bool, int, string) {
	t.Helper()
	rec := httptest.NewRecorder()
	handled := (&Provider{}).ServeLocalIndex(rec, httptest.NewRequest("GET", "/"+path, nil), fs, "go-local", path)
	return handled, rec.Code, rec.Body.String()
}

func TestServeLocalIndex(t *testing.T) {
	fs := &fakeFileStore{paths: []string{
		"myorg.example.net/hello/@v/v0.10.0.mod",
		"myorg.example.net/hello/@v/v0.10.0.zip",
		"myorg.example.net/hello/@v/v0.2.0.mod",
		"myorg.example.net/hello/@v/v0.2.0.zip",
		"myorg.example.net/hello/@v/v0.2.0.info",
		"myorg.example.net/hello/@v/v0.11.0-rc.1.mod",
		"myorg.example.net/hello/@v/v0.3.0.zip",
		"myorg.example.net/hello/v2/@v/v2.0.0.mod",
		"myorg.example.net/!pre/@v/v1.0.0-beta.mod",
	}}

	if h, code, body := serve(t, fs, "myorg.example.net/hello/@v/list"); !h || code != 200 || body != "v0.2.0\nv0.10.0\nv0.11.0-rc.1\n" {
		t.Errorf("list: %v %d %q", h, code, body)
	}
	if h, code, body := serve(t, fs, "myorg.example.net/hello/@latest"); !h || code != 200 || body != `{"Version":"v0.10.0"}` {
		t.Errorf("latest: %v %d %q", h, code, body)
	}
	if h, _, body := serve(t, fs, "myorg.example.net/!pre/@latest"); !h || body != `{"Version":"v1.0.0-beta"}` {
		t.Errorf("prerelease-only latest: %q", body)
	}
	if h, code, _ := serve(t, fs, "myorg.example.net/missing/@latest"); !h || code != 404 {
		t.Errorf("missing latest: %v %d", h, code)
	}
	if h, code, body := serve(t, fs, "myorg.example.net/missing/@v/list"); !h || code != 200 || body != "" {
		t.Errorf("missing list: %v %d %q", h, code, body)
	}
	if h, _, body := serve(t, fs, "myorg.example.net/hello/@v/v0.10.0.info"); !h || body != `{"Version":"v0.10.0"}` {
		t.Errorf("synthesized info: %v %q", h, body)
	}
	if h, _, _ := serve(t, fs, "myorg.example.net/hello/@v/v0.2.0.info"); h {
		t.Error("uploaded .info must be served from storage")
	}
	if h, _, _ := serve(t, fs, "myorg.example.net/hello/@v/v0.3.0.info"); h {
		t.Error(".info without a .mod must not be synthesized")
	}
	if h, _, _ := serve(t, fs, "myorg.example.net/hello/@v/v0.10.0.zip"); h {
		t.Error(".zip must be served from storage")
	}
}
