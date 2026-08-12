package alpine

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha1"
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/IUCLabs/artifactapi/internal/provider"
	"github.com/IUCLabs/artifactapi/internal/testsupport"
)

type fakeBlobReader struct{ data []byte }

func (f fakeBlobReader) Download(_ context.Context, _ string) (io.ReadCloser, int64, error) {
	return io.NopCloser(bytes.NewReader(f.data)), int64(len(f.data)), nil
}

type errBlobReader struct{}

func (errBlobReader) Download(_ context.Context, _ string) (io.ReadCloser, int64, error) {
	return nil, 0, io.ErrUnexpectedEOF
}

// fakeAlpineStore satisfies provider.MetadataStore (shared) and
// provider.AlpineMetadataStore, recording the row AfterUpload writes.
type fakeAlpineStore struct{ inserted *provider.AlpineMetadata }

func (f *fakeAlpineStore) InsertRPMMetadata(context.Context, *provider.RPMMetadata) error { return nil }
func (f *fakeAlpineStore) InsertDebMetadata(context.Context, *provider.DebMetadata) error { return nil }
func (f *fakeAlpineStore) InsertAlpineMetadata(_ context.Context, m *provider.AlpineMetadata) error {
	f.inserted = m
	return nil
}

// fakeAlpineDeleter satisfies provider.MetadataDeleter and AlpineMetadataDeleter.
type fakeAlpineDeleter struct{ deleted bool }

func (f *fakeAlpineDeleter) DeleteRPMMetadata(context.Context, string, string) error { return nil }
func (f *fakeAlpineDeleter) DeleteDebMetadata(context.Context, string, string) error { return nil }
func (f *fakeAlpineDeleter) DeleteAlpineMetadata(context.Context, string, string) error {
	f.deleted = true
	return nil
}

// fakeAlpineReader is a FileStore that also serves alpine metadata rows.
type fakeAlpineReader struct{ metas []provider.AlpineMetadata }

func (f fakeAlpineReader) ListAlpineMetadataEntries(context.Context, string) ([]provider.AlpineMetadata, error) {
	return f.metas, nil
}
func (f fakeAlpineReader) ListFilesByPrefix(context.Context, string, string) ([]provider.FileEntry, error) {
	return nil, nil
}
func (f fakeAlpineReader) ListPackages(context.Context, string) ([]string, error) { return nil, nil }

type errAlpineReader struct{}

func (errAlpineReader) ListAlpineMetadataEntries(context.Context, string) ([]provider.AlpineMetadata, error) {
	return nil, io.ErrUnexpectedEOF
}
func (errAlpineReader) ListFilesByPrefix(context.Context, string, string) ([]provider.FileEntry, error) {
	return nil, nil
}
func (errAlpineReader) ListPackages(context.Context, string) ([]string, error) { return nil, nil }

func TestAlpineValidateUpload(t *testing.T) {
	p := &Provider{}
	sp, ct, err := p.ValidateUpload("x86_64/foo-1.0-r0.apk")
	if err != nil || sp != "x86_64/foo-1.0-r0.apk" || ct != "application/vnd.android.package-archive" {
		t.Errorf("sp=%q ct=%q err=%v", sp, ct, err)
	}
	// Dot-segment prefix is normalized away.
	if sp, _, err := p.ValidateUpload("./aarch64/bar-2.0-r1.apk"); err != nil || sp != "aarch64/bar-2.0-r1.apk" {
		t.Errorf("dot-seg: sp=%q err=%v", sp, err)
	}
	if _, _, err := p.ValidateUpload("foo.rpm"); err == nil {
		t.Error("expected error for non-apk")
	}
	resp := p.UploadResponse("x86_64/foo-1.0-r0.apk", "sha256:abc", 42)
	if resp["filename"] != "foo-1.0-r0.apk" || resp["content_hash"] != "sha256:abc" || resp["size_bytes"] != int64(42) {
		t.Errorf("upload response %v", resp)
	}
}

func TestAlpineAfterUpload(t *testing.T) {
	data := testsupport.MinimalApk("hello", "1.0-r0", "x86_64")
	store := &fakeAlpineStore{}
	(&Provider{}).AfterUpload(context.Background(), "myrepo", "x86_64/hello-1.0-r0.apk",
		"sha256:deadbeef", fakeBlobReader{data: data}, store)

	m := store.inserted
	if m == nil {
		t.Fatal("no metadata inserted")
	}
	if m.Name != "hello" || m.Version != "1.0-r0" || m.Arch != "x86_64" {
		t.Errorf("unexpected metadata: %+v", m)
	}
	if m.DownloadSize != int64(len(data)) {
		t.Errorf("DownloadSize = %d, want %d", m.DownloadSize, len(data))
	}
	if m.InstalledSize != 4 {
		t.Errorf("InstalledSize = %d, want 4", m.InstalledSize)
	}
	if m.License != "MIT" || m.Origin != "hello" || !strings.HasPrefix(m.Maintainer, "e2e") {
		t.Errorf("scalar fields not parsed: %+v", m)
	}
	if len(m.Depends) != 1 || m.Depends[0] != "so:libc.musl-x86_64.so.1" {
		t.Errorf("Depends = %v", m.Depends)
	}
	if len(m.Provides) != 1 || m.Provides[0] != "cmd:hello=1.0-r0" {
		t.Errorf("Provides = %v", m.Provides)
	}

	// The Q1 checksum is the sha1 of the CONTROL gzip stream (the member whose
	// tar carries .PKGINFO), not of the whole file.
	controlRaw := controlStreamBytes(t, data)
	sum := sha1.Sum(controlRaw)
	want := "Q1" + base64.StdEncoding.EncodeToString(sum[:])
	if m.Checksum != want {
		t.Errorf("Checksum = %q, want %q (sha1 of control stream)", m.Checksum, want)
	}
	// And explicitly NOT the sha1 of the whole apk.
	whole := sha1.Sum(data)
	if m.Checksum == "Q1"+base64.StdEncoding.EncodeToString(whole[:]) {
		t.Error("Checksum was computed over the whole file, not the control stream")
	}
}

func TestAlpineAfterUploadErrors(t *testing.T) {
	store := &fakeAlpineStore{}
	(&Provider{}).AfterUpload(context.Background(), "r", "x86_64/p.apk", "sha256:x", errBlobReader{}, store)
	if store.inserted != nil {
		t.Error("no metadata should be inserted on download error")
	}
	store2 := &fakeAlpineStore{}
	(&Provider{}).AfterUpload(context.Background(), "r", "x86_64/p.apk", "sha256:x", fakeBlobReader{data: []byte("not an apk")}, store2)
	if store2.inserted != nil {
		t.Error("no metadata should be inserted on parse error")
	}
}

func TestAlpineAfterDelete(t *testing.T) {
	d := &fakeAlpineDeleter{}
	if err := (&Provider{}).AfterDelete(context.Background(), "r", "x86_64/p.apk", d); err != nil {
		t.Fatalf("AfterDelete: %v", err)
	}
	if !d.deleted {
		t.Error("DeleteAlpineMetadata not called")
	}
}

func TestAlpineServeLocalIndex(t *testing.T) {
	p := &Provider{}
	reader := fakeAlpineReader{metas: []provider.AlpineMetadata{
		{Name: "aaa", Version: "1.0-r0", Arch: "x86_64", Checksum: "Q1aaa", DownloadSize: 100, InstalledSize: 10,
			Description: "pkg aaa", URL: "https://a", License: "MIT", Depends: []string{"so:libc"}, Provides: []string{"cmd:aaa"}},
		{Name: "bbb", Version: "2.0-r0", Arch: "aarch64", Checksum: "Q1bbb", DownloadSize: 200, InstalledSize: 20},
	}}

	// x86_64 index contains only aaa, with its fields, and not bbb.
	w := serveIndex(t, p, reader, "x86_64/APKINDEX.tar.gz")
	if w.Code != 200 {
		t.Fatalf("code %d", w.Code)
	}
	idx := untarIndex(t, w.Body.Bytes())
	for _, want := range []string{"C:Q1aaa", "P:aaa", "V:1.0-r0", "A:x86_64", "S:100", "I:10", "T:pkg aaa", "U:https://a", "L:MIT", "D:so:libc", "p:cmd:aaa"} {
		if !strings.Contains(idx, want) {
			t.Errorf("x86_64 APKINDEX missing %q:\n%s", want, idx)
		}
	}
	if strings.Contains(idx, "P:bbb") {
		t.Errorf("x86_64 APKINDEX leaked aarch64 package:\n%s", idx)
	}

	// aarch64 index contains only bbb.
	w = serveIndex(t, p, reader, "aarch64/APKINDEX.tar.gz")
	idx = untarIndex(t, w.Body.Bytes())
	if !strings.Contains(idx, "P:bbb") || strings.Contains(idx, "P:aaa") {
		t.Errorf("aarch64 filtering wrong:\n%s", idx)
	}

	// Non-index and .apk paths are not owned by the indexer.
	for _, path := range []string{"x86_64/foo-1.0-r0.apk", "x86_64/", "README"} {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodGet, "/"+path, nil)
		if p.ServeLocalIndex(w, r, reader, "repo", path) {
			t.Errorf("ServeLocalIndex should return false for %q", path)
		}
	}
}

// Empty fields are omitted from the record (bbb has no description/url).
func TestAlpineIndexOmitsEmptyFields(t *testing.T) {
	p := &Provider{}
	reader := fakeAlpineReader{metas: []provider.AlpineMetadata{
		{Name: "bbb", Version: "2.0-r0", Arch: "x86_64", Checksum: "Q1bbb", DownloadSize: 200, InstalledSize: 20},
	}}
	idx := untarIndex(t, serveIndex(t, p, reader, "x86_64/APKINDEX.tar.gz").Body.Bytes())
	for _, absent := range []string{"T:", "U:", "L:", "D:", "p:", "i:", "o:", "m:", "c:", "k:"} {
		if strings.Contains(idx, absent) {
			t.Errorf("empty field %q should be omitted:\n%s", absent, idx)
		}
	}
}

// apk requests "./<arch>/APKINDEX.tar.gz" for a bare repo base URL; the
// dot-segment must be collapsed and yield the same bytes as the plain path.
func TestAlpineServeLocalIndexDotSegment(t *testing.T) {
	p := &Provider{}
	reader := fakeAlpineReader{metas: []provider.AlpineMetadata{
		{Name: "aaa", Version: "1.0-r0", Arch: "x86_64", Checksum: "Q1aaa", DownloadSize: 100, InstalledSize: 10},
	}}
	plain := untarIndex(t, serveIndex(t, p, reader, "x86_64/APKINDEX.tar.gz").Body.Bytes())
	dotted := untarIndex(t, serveIndex(t, p, reader, "./x86_64/APKINDEX.tar.gz").Body.Bytes())
	if plain != dotted {
		t.Errorf("dot-segment path differs:\nplain=%q\ndotted=%q", plain, dotted)
	}
}

func TestAlpineServeLocalIndexArchRequired(t *testing.T) {
	p := &Provider{}
	reader := fakeAlpineReader{}
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/APKINDEX.tar.gz", nil)
	if !p.ServeLocalIndex(w, r, reader, "repo", "APKINDEX.tar.gz") {
		t.Fatal("bare APKINDEX should be owned (and rejected) by the indexer")
	}
	if w.Code != http.StatusNotFound {
		t.Errorf("bare APKINDEX code = %d, want 404", w.Code)
	}
}

func TestAlpineServeMetadataError(t *testing.T) {
	p := &Provider{}
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/x86_64/APKINDEX.tar.gz", nil)
	p.ServeLocalIndex(w, r, errAlpineReader{}, "repo", "x86_64/APKINDEX.tar.gz")
	if w.Code != 500 {
		t.Errorf("failing reader code = %d, want 500", w.Code)
	}
}

func TestAlpineGenerateLocalIndexUnsupported(t *testing.T) {
	if _, err := (&Provider{}).GenerateLocalIndex(context.Background(), fakeAlpineReader{}, "r", "x86_64/APKINDEX.tar.gz"); err == nil {
		t.Error("expected unsupported error")
	}
}

func serveIndex(t *testing.T, p *Provider, files provider.FileStore, path string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/"+path, nil)
	if !p.ServeLocalIndex(w, r, files, "repo", path) {
		t.Fatalf("ServeLocalIndex returned false for %q", path)
	}
	return w
}

// untarIndex un-gzips and un-tars an APKINDEX.tar.gz and returns the APKINDEX text.
func untarIndex(t *testing.T, gzTar []byte) string {
	t.Helper()
	zr, err := gzip.NewReader(bytes.NewReader(gzTar))
	if err != nil {
		t.Fatalf("APKINDEX not gzip: %v", err)
	}
	tarBytes, _ := io.ReadAll(zr)
	tr := tar.NewReader(bytes.NewReader(tarBytes))
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("APKINDEX not tar: %v", err)
		}
		if hdr.Name == "APKINDEX" {
			b, _ := io.ReadAll(tr)
			return string(b)
		}
	}
	t.Fatal("no APKINDEX member in tarball")
	return ""
}

// controlStreamBytes returns the raw bytes of the gzip stream whose tar carries
// .PKGINFO, so the test can independently compute the expected Q1 checksum.
func controlStreamBytes(t *testing.T, apk []byte) []byte {
	t.Helper()
	br := bytes.NewReader(apk)
	zr, err := gzip.NewReader(br)
	if err != nil {
		t.Fatalf("gzip: %v", err)
	}
	prev := 0
	for {
		zr.Multistream(false)
		out, _ := io.ReadAll(zr)
		end := len(apk) - br.Len()
		tr := tar.NewReader(bytes.NewReader(out))
		for {
			h, err := tr.Next()
			if err != nil {
				break
			}
			if strings.TrimPrefix(h.Name, "./") == ".PKGINFO" {
				return apk[prev:end]
			}
		}
		prev = end
		if err := zr.Reset(br); err != nil {
			break
		}
	}
	t.Fatal("no control stream found")
	return nil
}
