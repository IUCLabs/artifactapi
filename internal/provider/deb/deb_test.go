package deb

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/klauspost/compress/zstd"
	"github.com/ulikunitz/xz"

	"github.com/IUCLabs/artifactapi/internal/provider"
	"github.com/IUCLabs/artifactapi/internal/testsupport"
	"github.com/IUCLabs/artifactapi/pkg/models"
)

type fakeBlobReader struct{ data []byte }

func (f fakeBlobReader) Download(_ context.Context, _ string) (io.ReadCloser, int64, error) {
	return io.NopCloser(bytes.NewReader(f.data)), int64(len(f.data)), nil
}

type errBlobReader struct{}

func (errBlobReader) Download(_ context.Context, _ string) (io.ReadCloser, int64, error) {
	return nil, 0, io.ErrUnexpectedEOF
}

// fakeDebStore satisfies provider.MetadataStore (both insert methods) and
// records the deb row that AfterUpload writes.
type fakeDebStore struct{ inserted *provider.DebMetadata }

func (f *fakeDebStore) InsertRPMMetadata(context.Context, *provider.RPMMetadata) error { return nil }
func (f *fakeDebStore) InsertDebMetadata(_ context.Context, m *provider.DebMetadata) error {
	f.inserted = m
	return nil
}

type fakeDebReader struct{ metas []provider.DebMetadata }

func (f fakeDebReader) ListDebMetadataEntries(context.Context, string) ([]provider.DebMetadata, error) {
	return f.metas, nil
}
func (f fakeDebReader) ListFilesByPrefix(context.Context, string, string) ([]provider.FileEntry, error) {
	return nil, nil
}
func (f fakeDebReader) ListPackages(context.Context, string) ([]string, error) { return nil, nil }

type errDebReader struct{}

func (errDebReader) ListDebMetadataEntries(context.Context, string) ([]provider.DebMetadata, error) {
	return nil, io.ErrUnexpectedEOF
}
func (errDebReader) ListFilesByPrefix(context.Context, string, string) ([]provider.FileEntry, error) {
	return nil, nil
}
func (errDebReader) ListPackages(context.Context, string) ([]string, error) { return nil, nil }

func TestDebPureFuncs(t *testing.T) {
	p := &Provider{}
	if p.Type() != models.PackageDeb {
		t.Errorf("type = %q", p.Type())
	}
	if out, _ := p.RewriteResponse(nil, models.Remote{}, "http://p"); out != nil {
		t.Error("deb never rewrites")
	}
	if got := p.UpstreamURL(models.Remote{BaseURL: "https://mirror/"}, "/dists/bookworm/Release"); got != "https://mirror/dists/bookworm/Release" {
		t.Errorf("upstream url %q", got)
	}
	h, _ := p.AuthHeaders(context.Background(), models.Remote{Username: "u", Password: "p"})
	if h.Get("Authorization") == "" {
		t.Error("auth header")
	}
}

func TestDebClassify(t *testing.T) {
	p := &Provider{}
	tests := []struct {
		path string
		want provider.Mutability
	}{
		{"pool/foo_1.0_amd64.deb", provider.Immutable},
		{"Packages", provider.Mutable},
		{"Packages.gz", provider.Mutable},
		{"Release", provider.Mutable},
		{"InRelease", provider.Mutable},
		{"Release.gpg", provider.Mutable},
		{"dists/bookworm/main/binary-amd64/Packages", provider.Mutable},
		{"dists/bookworm/Release", provider.Mutable},
		{"dists/bookworm/main/by-hash/SHA256/abc", provider.Mutable},
		{"dists/bookworm/main/Contents-amd64.gz", provider.Mutable},
	}
	for _, tt := range tests {
		if got := p.Classify(tt.path); got != tt.want {
			t.Errorf("Classify(%q) = %v, want %v", tt.path, got, tt.want)
		}
	}
}

func TestDebContentType(t *testing.T) {
	p := &Provider{}
	for path, want := range map[string]string{
		"pool/foo_1.0_amd64.deb":              "application/vnd.debian.binary-package",
		"dists/bookworm/main/bin/Packages.gz": "application/gzip",
		"dists/bookworm/main/bin/Packages.xz": "application/x-xz",
		"Packages":                            "text/plain",
		"Release":                             "text/plain",
		"InRelease":                           "text/plain",
		"pool/other":                          "application/octet-stream",
	} {
		if got := p.ContentType(path); got != want {
			t.Errorf("ContentType(%q) = %q, want %q", path, got, want)
		}
	}
}

func TestDebValidateUpload(t *testing.T) {
	p := &Provider{}
	sp, ct, err := p.ValidateUpload("dir/foo_1.0_amd64.deb")
	if err != nil || sp != "pool/foo_1.0_amd64.deb" || ct != "application/vnd.debian.binary-package" {
		t.Errorf("sp=%q ct=%q err=%v", sp, ct, err)
	}
	if _, _, err := p.ValidateUpload("foo.rpm"); err == nil {
		t.Error("expected error for non-deb")
	}
	resp := p.UploadResponse("pool/foo_1.0_amd64.deb", "sha256:abc", 42)
	if resp["filename"] != "foo_1.0_amd64.deb" || resp["content_hash"] != "sha256:abc" || resp["size_bytes"] != int64(42) {
		t.Errorf("upload response %v", resp)
	}
}

func TestDebAfterUpload(t *testing.T) {
	data := testsupport.MinimalDeb("e2e-testpkg", "1.2.3", "amd64")
	store := &fakeDebStore{}
	(&Provider{}).AfterUpload(context.Background(), "myrepo", "pool/e2e-testpkg_1.2.3_amd64.deb",
		"sha256:deadbeef", fakeBlobReader{data: data}, store)

	m := store.inserted
	if m == nil {
		t.Fatal("no metadata inserted")
	}
	if m.Name != "e2e-testpkg" || m.Version != "1.2.3" || m.Architecture != "amd64" {
		t.Errorf("unexpected metadata: %+v", m)
	}
	if m.Size != int64(len(data)) {
		t.Errorf("Size = %d, want %d", m.Size, len(data))
	}
	if m.SHA256 != "deadbeef" {
		t.Errorf("SHA256 = %q, want deadbeef", m.SHA256)
	}
	if m.MD5 == "" {
		t.Error("MD5 not computed")
	}
	if !strings.Contains(m.Control, "Package: e2e-testpkg") {
		t.Errorf("raw control not stored: %q", m.Control)
	}
	// The raw stanza is stored verbatim (no trailing newline) so Packages can
	// reproduce it faithfully.
	if strings.HasSuffix(m.Control, "\n") {
		t.Error("control should be trimmed of trailing newline")
	}
}

func TestDebAfterUploadErrors(t *testing.T) {
	// Download failure: no insert, no panic.
	store := &fakeDebStore{}
	(&Provider{}).AfterUpload(context.Background(), "r", "p", "sha256:x", errBlobReader{}, store)
	if store.inserted != nil {
		t.Error("no metadata should be inserted on download error")
	}
	// Not a .deb (ar) archive.
	store2 := &fakeDebStore{}
	(&Provider{}).AfterUpload(context.Background(), "r", "p", "sha256:x", fakeBlobReader{data: []byte("not a deb")}, store2)
	if store2.inserted != nil {
		t.Error("no metadata should be inserted on parse error")
	}
}

func TestDebControlDecompression(t *testing.T) {
	// The control tarball may be gzip, xz, or zstd (goreleaser/nfpm emit gzip or
	// xz); each must round-trip to the same control stanza.
	for _, tc := range []struct {
		name   string
		member string
		comp   func([]byte) []byte
	}{
		{"gzip", "control.tar.gz", gzipBytes},
		{"xz", "control.tar.xz", xzBytes},
		{"zstd", "control.tar.zst", zstdBytes},
	} {
		t.Run(tc.name, func(t *testing.T) {
			deb := buildDeb("pkg", "9.9", "arm64", tc.member, tc.comp)
			control, err := extractControl(deb)
			if err != nil {
				t.Fatalf("extractControl: %v", err)
			}
			fields := parseControlFields(control)
			if fields["Package"] != "pkg" || fields["Version"] != "9.9" || fields["Architecture"] != "arm64" {
				t.Errorf("fields = %v", fields)
			}
		})
	}
}

func TestDebParseControlContinuationLines(t *testing.T) {
	control := "Package: p\nVersion: 1\n" +
		"Description: short\n very long\n .\n more\n" +
		"Architecture: all\n"
	f := parseControlFields(control)
	if f["Package"] != "p" || f["Version"] != "1" || f["Architecture"] != "all" {
		t.Errorf("continuation lines corrupted parse: %v", f)
	}
	if f["Description"] != "short" {
		t.Errorf("Description folded continuation into value: %q", f["Description"])
	}
}

func TestDebServeLocalIndex(t *testing.T) {
	p := &Provider{}
	reader := fakeDebReader{metas: []provider.DebMetadata{
		{Name: "aaa", Version: "1.0", Architecture: "amd64", FilePath: "pool/aaa_1.0_amd64.deb",
			Control: "Package: aaa\nVersion: 1.0\nArchitecture: amd64", Size: 100, MD5: "md5aaa", SHA256: "sha256aaa"},
		{Name: "bbb", Version: "2.0", Architecture: "arm64", FilePath: "pool/bbb_2.0_arm64.deb",
			Control: "Package: bbb\nVersion: 2.0\nArchitecture: arm64", Size: 200, MD5: "md5bbb", SHA256: "sha256bbb"},
	}}

	serve := func(path string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodGet, "/"+path, nil)
		if !p.ServeLocalIndex(w, r, reader, "myrepo", path) {
			t.Fatalf("ServeLocalIndex returned false for %q", path)
		}
		return w
	}

	// Packages lists both packages with their apt fields.
	w := serve("Packages")
	body := w.Body.String()
	if w.Code != 200 {
		t.Fatalf("Packages code %d", w.Code)
	}
	for _, want := range []string{
		"Package: aaa", "Package: bbb",
		"Filename: pool/aaa_1.0_amd64.deb", "Size: 100", "MD5sum: md5aaa", "SHA256: sha256aaa",
		"Filename: pool/bbb_2.0_arm64.deb", "Size: 200",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("Packages missing %q:\n%s", want, body)
		}
	}
	// Stanzas are blank-line separated.
	if !strings.Contains(body, "SHA256: sha256aaa\n\n") {
		t.Errorf("stanzas not blank-line separated:\n%s", body)
	}

	// Packages.gz decompresses to exactly the plain Packages bytes.
	w = serve("Packages.gz")
	if w.Code != 200 {
		t.Fatalf("Packages.gz code %d", w.Code)
	}
	zr, err := gzip.NewReader(bytes.NewReader(w.Body.Bytes()))
	if err != nil {
		t.Fatalf("Packages.gz not gzip: %v", err)
	}
	plain, _ := io.ReadAll(zr)
	if !bytes.Equal(plain, []byte(body)) {
		t.Error("Packages.gz does not decompress to Packages")
	}

	// Release lists arches and both index files under MD5Sum/SHA256.
	w = serve("Release")
	rel := w.Body.String()
	if w.Code != 200 {
		t.Fatalf("Release code %d", w.Code)
	}
	for _, want := range []string{"Date:", "Architectures: amd64 arm64", "Acquire-By-Hash: no", "MD5Sum:", "SHA256:", " Packages\n", " Packages.gz\n"} {
		if !strings.Contains(rel, want) {
			t.Errorf("Release missing %q:\n%s", want, rel)
		}
	}

	// Unsigned trust model: no InRelease / Release.gpg served here.
	for _, path := range []string{"InRelease", "Release.gpg", "pool/aaa_1.0_amd64.deb"} {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodGet, "/"+path, nil)
		if p.ServeLocalIndex(w, r, reader, "myrepo", path) {
			t.Errorf("ServeLocalIndex should return false for %q", path)
		}
	}
}

// Real apt appends the flat-repo dist "./" verbatim, so it requests "./Packages"
// / "./Release" (curl pre-normalizes /./ which masks this). The handler must
// collapse the dot-segment and return the same bytes as the un-prefixed request.
func TestDebServeLocalIndexAptDotSegment(t *testing.T) {
	p := &Provider{}
	reader := fakeDebReader{metas: []provider.DebMetadata{
		{Name: "aaa", Version: "1.0", Architecture: "amd64", FilePath: "pool/aaa_1.0_amd64.deb",
			Control: "Package: aaa\nVersion: 1.0\nArchitecture: amd64", Size: 100, MD5: "md5aaa", SHA256: "sha256aaa"},
	}}

	serve := func(path string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodGet, "/"+path, nil)
		if !p.ServeLocalIndex(w, r, reader, "myrepo", path) {
			t.Fatalf("ServeLocalIndex returned false for %q", path)
		}
		return w
	}

	// Packages is deterministic: require exact byte identity.
	if plain, dotted := serve("Packages"), serve("./Packages"); plain.Code != 200 || dotted.Code != 200 {
		t.Fatalf("Packages: plain=%d dotted=%d, want 200/200", plain.Code, dotted.Code)
	} else if !bytes.Equal(plain.Body.Bytes(), dotted.Body.Bytes()) {
		t.Error("./Packages body differs from Packages body")
	}

	// Release carries a Date: header stamped from time.Now(); compare the rest.
	plain, dotted := serve("Release"), serve("./Release")
	if plain.Code != 200 || dotted.Code != 200 {
		t.Fatalf("Release: plain=%d dotted=%d, want 200/200", plain.Code, dotted.Code)
	}
	if stripDate(plain.Body.String()) != stripDate(dotted.Body.String()) {
		t.Error("./Release body differs from Release body (ignoring Date)")
	}
}

func stripDate(s string) string {
	var out []string
	for _, line := range strings.Split(s, "\n") {
		if strings.HasPrefix(line, "Date:") {
			continue
		}
		out = append(out, line)
	}
	return strings.Join(out, "\n")
}

func TestDebServeMetadataError(t *testing.T) {
	p := &Provider{}
	for _, path := range []string{"Packages", "Packages.gz", "Release"} {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodGet, "/"+path, nil)
		p.ServeLocalIndex(w, r, errDebReader{}, "repo", path)
		if w.Code != 500 {
			t.Errorf("%s with failing reader = %d, want 500", path, w.Code)
		}
	}
}

func TestDebGenerateLocalIndexUnsupported(t *testing.T) {
	if _, err := (&Provider{}).GenerateLocalIndex(context.Background(), fakeDebReader{}, "r", "Packages"); err == nil {
		t.Error("expected unsupported error")
	}
}

// buildDeb assembles an ar .deb whose control member uses the given name and
// compressor, so the decompression branches can be exercised directly.
func buildDeb(name, version, arch, member string, comp func([]byte) []byte) []byte {
	control := "Package: " + name + "\nVersion: " + version + "\nArchitecture: " + arch + "\n"
	controlTar := comp(tarSingle("./control", []byte(control)))

	var buf bytes.Buffer
	buf.WriteString("!<arch>\n")
	arWrite(&buf, "debian-binary", []byte("2.0\n"))
	arWrite(&buf, member, controlTar)
	arWrite(&buf, "data.tar.gz", gzipBytes(tarSingle("./x", []byte("x"))))
	return buf.Bytes()
}

func tarSingle(name string, data []byte) []byte {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(data)), Typeflag: tar.TypeReg})
	tw.Write(data)
	tw.Close()
	return buf.Bytes()
}

func arWrite(buf *bytes.Buffer, name string, data []byte) {
	fmt.Fprintf(buf, "%-16s%-12s%-6s%-6s%-8s%-10d`\n", name, "0", "0", "0", "100644", len(data))
	buf.Write(data)
	if len(data)%2 == 1 {
		buf.WriteByte('\n')
	}
}

func xzBytes(data []byte) []byte {
	var buf bytes.Buffer
	w, _ := xz.NewWriter(&buf)
	w.Write(data)
	w.Close()
	return buf.Bytes()
}

func zstdBytes(data []byte) []byte {
	var buf bytes.Buffer
	w, _ := zstd.NewWriter(&buf)
	w.Write(data)
	w.Close()
	return buf.Bytes()
}
