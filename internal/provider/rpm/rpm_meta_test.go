package rpm

import (
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/IUCLabs/artifactapi/internal/provider"
	"github.com/IUCLabs/artifactapi/internal/testsupport"
	"github.com/IUCLabs/artifactapi/pkg/models"
)

type fakeBlobReader struct{ data []byte }

func (f fakeBlobReader) Download(_ context.Context, _ string) (io.ReadCloser, int64, error) {
	return io.NopCloser(bytes.NewReader(f.data)), int64(len(f.data)), nil
}

type fakeMetaStore struct{ inserted *provider.RPMMetadata }

func (f *fakeMetaStore) InsertRPMMetadata(_ context.Context, m *provider.RPMMetadata) error {
	f.inserted = m
	return nil
}

type fakeRPMReader struct{ metas []provider.RPMMetadata }

func (f fakeRPMReader) ListRPMMetadataEntries(_ context.Context, _ string) ([]provider.RPMMetadata, error) {
	return f.metas, nil
}
func (f fakeRPMReader) ListFilesByPrefix(_ context.Context, _, _ string) ([]provider.FileEntry, error) {
	return nil, nil
}
func (f fakeRPMReader) ListPackages(_ context.Context, _ string) ([]string, error) { return nil, nil }

func TestRPMPureFuncs(t *testing.T) {
	p := &Provider{}
	if p.Type() != models.PackageRPM {
		t.Error("type")
	}
	if p.Classify("repodata/repomd.xml") != provider.Mutable {
		t.Error("repomd should be mutable")
	}
	if p.Classify("Packages/foo.rpm") != provider.Immutable {
		t.Error("rpm should be immutable")
	}
	if p.ContentType("x.rpm") != "application/x-rpm" {
		t.Error("rpm content type")
	}
	if got := p.UpstreamURL(models.Remote{BaseURL: "https://mirror/"}, "/Packages/x.rpm"); got != "https://mirror/Packages/x.rpm" {
		t.Errorf("upstream url %q", got)
	}
	if out, _ := p.RewriteResponse(nil, models.Remote{}, "http://p"); out != nil {
		t.Error("rpm never rewrites")
	}
	h, _ := p.AuthHeaders(context.Background(), models.Remote{Username: "u", Password: "p"})
	if h.Get("Authorization") == "" {
		t.Error("auth header")
	}
}

func TestRPMValidateUpload(t *testing.T) {
	p := &Provider{}
	sp, ct, err := p.ValidateUpload("dir/foo-1.0.noarch.rpm")
	if err != nil || sp != "Packages/foo-1.0.noarch.rpm" || ct != "application/x-rpm" {
		t.Errorf("sp=%q ct=%q err=%v", sp, ct, err)
	}
	if _, _, err := p.ValidateUpload("foo.txt"); err == nil {
		t.Error("expected error for non-rpm")
	}
	resp := p.UploadResponse("Packages/foo.rpm", "sha256:abc", 10)
	if resp["content_hash"] != "sha256:abc" {
		t.Errorf("upload response %v", resp)
	}
}

func TestRPMAfterUpload(t *testing.T) {
	data := testsupport.MinimalRPM("e2e-testpkg", "1.0", "1", "noarch")
	store := &fakeMetaStore{}
	(&Provider{}).AfterUpload(context.Background(), "myrepo", "Packages/e2e-testpkg-1.0-1.noarch.rpm",
		"sha256:deadbeef", fakeBlobReader{data: data}, store)

	m := store.inserted
	if m == nil {
		t.Fatal("no metadata inserted")
	}
	if m.Name != "e2e-testpkg" || m.Version != "1.0" || m.Release != "1" || m.Arch != "noarch" {
		t.Errorf("unexpected metadata: %+v", m)
	}
	if m.RPMSize != int64(len(data)) {
		t.Errorf("RPMSize = %d, want %d", m.RPMSize, len(data))
	}
	if len(m.Provides) == 0 {
		t.Error("expected the package to provide itself")
	}
}

type errBlobReader struct{}

func (errBlobReader) Download(_ context.Context, _ string) (io.ReadCloser, int64, error) {
	return nil, 0, io.ErrUnexpectedEOF
}

func TestRPMAfterUploadErrors(t *testing.T) {
	// Download failure: no metadata inserted, no panic.
	store := &fakeMetaStore{}
	(&Provider{}).AfterUpload(context.Background(), "r", "p", "sha256:x", errBlobReader{}, store)
	if store.inserted != nil {
		t.Error("no metadata should be inserted on download error")
	}
	// Parse failure: garbage bytes are not a valid RPM.
	store2 := &fakeMetaStore{}
	(&Provider{}).AfterUpload(context.Background(), "r", "p", "sha256:x", fakeBlobReader{data: []byte("not an rpm")}, store2)
	if store2.inserted != nil {
		t.Error("no metadata should be inserted on parse error")
	}
}

func TestRPMServeRepodata(t *testing.T) {
	p := &Provider{}
	reader := fakeRPMReader{metas: []provider.RPMMetadata{{
		Name: "e2e-testpkg", Version: "1.0", Release: "1", Arch: "noarch",
		Summary:     "test & <special>",
		ContentHash: "sha256:abc",
		Requires:    []provider.RPMDep{{Name: "libc", Flags: "GE", Version: "2.0"}},
		Provides:    []provider.RPMDep{{Name: "e2e-testpkg"}},
		Files:       []provider.RPMFile{{Path: "/usr/share/e2e/README", Type: "file"}},
		Changelogs:  []provider.RPMChangelog{{Author: "e2e", Date: 1, Text: "init"}},
	}}}

	serve := func(path string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodGet, "/"+path, nil)
		if !p.ServeLocalIndex(w, r, reader, "myrepo", path) {
			t.Fatalf("ServeLocalIndex returned false for %q", path)
		}
		return w
	}

	if w := serve("repodata/repomd.xml"); w.Code != 200 || !strings.Contains(w.Body.String(), "<repomd") {
		t.Errorf("repomd: code=%d body=%s", w.Code, w.Body.String())
	}
	for _, name := range []string{"repodata/h-primary.xml.gz", "repodata/h-filelists.xml.gz", "repodata/h-other.xml.gz"} {
		w := serve(name)
		if w.Code != 200 {
			t.Errorf("%s: code %d", name, w.Code)
		}
		if _, err := gzip.NewReader(bytes.NewReader(w.Body.Bytes())); err != nil {
			t.Errorf("%s: not gzip: %v", name, err)
		}
	}
	// Unknown repodata file -> 404.
	if w := serve("repodata/bogus"); w.Code != http.StatusNotFound {
		t.Errorf("bogus repodata: code %d", w.Code)
	}
	// Non-repodata path -> not handled.
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/Packages/x.rpm", nil)
	if p.ServeLocalIndex(w, r, reader, "myrepo", "Packages/x.rpm") {
		t.Error("expected ServeLocalIndex false for non-repodata path")
	}
}

type errRPMReader struct{}

func (errRPMReader) ListRPMMetadataEntries(context.Context, string) ([]provider.RPMMetadata, error) {
	return nil, io.ErrUnexpectedEOF
}
func (errRPMReader) ListFilesByPrefix(context.Context, string, string) ([]provider.FileEntry, error) {
	return nil, nil
}
func (errRPMReader) ListPackages(context.Context, string) ([]string, error) { return nil, nil }

func TestRPMServeMetadataError(t *testing.T) {
	p := &Provider{}
	for _, path := range []string{"repodata/repomd.xml", "repodata/h-primary.xml.gz", "repodata/h-filelists.xml.gz", "repodata/h-other.xml.gz"} {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodGet, "/"+path, nil)
		p.ServeLocalIndex(w, r, errRPMReader{}, "repo", path)
		if w.Code != 500 {
			t.Errorf("%s with failing reader = %d, want 500", path, w.Code)
		}
	}
}

func TestRPMFullMetadataXML(t *testing.T) {
	// A fully-populated entry exercises every optional-field branch in the
	// primary/filelists/other XML generators.
	metas := []provider.RPMMetadata{{
		Name: "full", Epoch: 1, Version: "2.0", Release: "3", Arch: "x86_64",
		Summary: "s", Description: "d", License: "MIT", Vendor: "acme",
		Group: "System", BuildHost: "build.example.com", SourceRPM: "full-2.0.src.rpm",
		URL: "https://example.com", Packager: "pkgr", ContentHash: "sha256:abc",
		RPMSize: 100, InstalledSize: 200,
		Requires:   []provider.RPMDep{{Name: "libc", Flags: "GE", Epoch: "0", Version: "2.0", Release: "1"}},
		Provides:   []provider.RPMDep{{Name: "full", Flags: "EQ", Version: "2.0"}},
		Files:      []provider.RPMFile{{Path: "/usr/bin/full", Type: "file"}, {Path: "/etc/full", Type: "dir"}},
		Changelogs: []provider.RPMChangelog{{Author: "a", Date: 100, Text: "changed"}},
	}}
	for _, gen := range []func([]provider.RPMMetadata) []byte{generatePrimaryXMLGZ, generateFilelistsXMLGZ, generateOtherXMLGZ} {
		zr, err := gzip.NewReader(bytes.NewReader(gen(metas)))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.ReadAll(zr); err != nil {
			t.Error(err)
		}
	}
}

func TestRPMPrimaryXMLContents(t *testing.T) {
	// Exercise xmlEscape and dependency entry writing through the gzip'd XML.
	metas := []provider.RPMMetadata{{
		Name: "pkg", Version: "1", Release: "1", Arch: "x86_64", Summary: "a & b",
		Requires: []provider.RPMDep{{Name: "dep", Flags: "EQ", Version: "1.0", Epoch: "0"}},
	}}
	gz := generatePrimaryXMLGZ(metas)
	zr, err := gzip.NewReader(bytes.NewReader(gz))
	if err != nil {
		t.Fatal(err)
	}
	out, _ := io.ReadAll(zr)
	s := string(out)
	if !strings.Contains(s, "a &amp; b") {
		t.Errorf("summary not xml-escaped: %s", s)
	}
	if !strings.Contains(s, "<name>pkg</name>") {
		t.Errorf("package name missing: %s", s)
	}
}

func TestRPMContentTypeAndHelpers(t *testing.T) {
	p := &Provider{}
	for path, want := range map[string]string{
		"x.rpm":                     "application/x-rpm",
		"repodata/repomd.xml":       "application/xml",
		"repodata/h-primary.xml.gz": "application/xml",
		"repodata/h-primary.xml.xz": "application/xml",
		"Packages/other":            "application/octet-stream",
	} {
		if got := p.ContentType(path); got != want {
			t.Errorf("ContentType(%q)=%q want %q", path, got, want)
		}
	}

	for flag, want := range map[int]string{
		0x08 | 0x04: "GE",
		0x02 | 0x04: "LE",
		0x08:        "GT",
		0x02:        "LT",
		0x04:        "EQ",
		0x00:        "",
	} {
		if got := rpmFlagString(flag); got != want {
			t.Errorf("rpmFlagString(%d)=%q want %q", flag, got, want)
		}
	}

	if firstGroup(nil) != "Unspecified" {
		t.Error("empty groups should be Unspecified")
	}
	if firstGroup([]string{"System", "Base"}) != "System" {
		t.Error("firstGroup should return the first")
	}
}

func TestGenerateLocalIndexUnsupported(t *testing.T) {
	if _, err := (&Provider{}).GenerateLocalIndex(context.Background(), fakeRPMReader{}, "r", "simple/"); err == nil {
		t.Error("expected unsupported error")
	}
}
