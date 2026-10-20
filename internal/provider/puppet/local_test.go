package puppet

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/IUCLabs/artifactapi/internal/provider"
)

func moduleTarball(t *testing.T, dir, metadata string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	for name, body := range map[string]string{dir + "/metadata.json": metadata, dir + "/manifests/init.pp": "class hello {}\n"} {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(body))}); err != nil {
			t.Fatal(err)
		}
		_, _ = tw.Write([]byte(body))
	}
	_ = tw.Close()
	_ = zw.Close()
	return buf.Bytes()
}

const helloMetadata = `{"name":"myorg-hello","version":"0.1.0","dependencies":[{"name":"puppetlabs/stdlib","version_requirement":">= 4.0.0"}]}`

type fakeBlobs struct{ data []byte }

func (f fakeBlobs) Download(context.Context, string) (io.ReadCloser, int64, error) {
	return io.NopCloser(bytes.NewReader(f.data)), int64(len(f.data)), nil
}

type fakeStore struct{ inserted *provider.PuppetMetadata }

func (f *fakeStore) InsertRPMMetadata(context.Context, *provider.RPMMetadata) error { return nil }
func (f *fakeStore) InsertDebMetadata(context.Context, *provider.DebMetadata) error { return nil }
func (f *fakeStore) InsertPuppetMetadata(_ context.Context, m *provider.PuppetMetadata) error {
	f.inserted = m
	return nil
}

type fakeReader struct{ metas []provider.PuppetMetadata }

func (f fakeReader) ListPuppetMetadataEntries(_ context.Context, _, slug string) ([]provider.PuppetMetadata, error) {
	var out []provider.PuppetMetadata
	for _, m := range f.metas {
		if slug == "" || m.Slug == slug {
			out = append(out, m)
		}
	}
	return out, nil
}
func (fakeReader) ListFilesByPrefix(context.Context, string, string) ([]provider.FileEntry, error) {
	return nil, nil
}
func (fakeReader) ListPackages(context.Context, string) ([]string, error) { return nil, nil }

func TestValidateUpload(t *testing.T) {
	p := &Provider{}
	sp, ct, err := p.ValidateUpload("some/dir/Myorg-hello-0.1.0.tar.gz")
	if err != nil || sp != "v3/files/myorg-hello-0.1.0.tar.gz" || ct != "application/gzip" {
		t.Errorf("sp=%q ct=%q err=%v", sp, ct, err)
	}
	for _, bad := range []string{"hello-0.1.0.tar.gz", "myorg-hello-0.1.tar.gz", "myorg-hello-0.1.0.zip", "myorg-Hello_x-0.1.0.tgz"} {
		if _, _, err := p.ValidateUpload(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestAfterUploadRecordsMetadata(t *testing.T) {
	raw := moduleTarball(t, "myorg-hello-0.1.0", helloMetadata)
	store := &fakeStore{}
	(&Provider{}).AfterUpload(context.Background(), "forge", "v3/files/myorg-hello-0.1.0.tar.gz", "sha256:abc", fakeBlobs{raw}, store)
	m := store.inserted
	if m == nil {
		t.Fatal("nothing inserted")
	}
	sum := md5.Sum(raw)
	if m.Slug != "myorg-hello" || m.Version != "0.1.0" || m.MD5 != hex.EncodeToString(sum[:]) || m.Size != int64(len(raw)) || m.MetadataJSON != helloMetadata {
		t.Errorf("got %+v", m)
	}
}

func TestAfterUploadRejectsMismatchedFilename(t *testing.T) {
	raw := moduleTarball(t, "myorg-hello-0.2.0", `{"name":"myorg-hello","version":"0.2.0"}`)
	store := &fakeStore{}
	(&Provider{}).AfterUpload(context.Background(), "forge", "v3/files/myorg-hello-0.1.0.tar.gz", "sha256:abc", fakeBlobs{raw}, store)
	if store.inserted != nil {
		t.Error("mismatched version indexed")
	}
}

func TestParseModuleRequiresTopLevelMetadata(t *testing.T) {
	if _, err := parseModule(bytes.NewReader(moduleTarball(t, "a/b", helloMetadata))); err == nil {
		t.Error("nested metadata.json accepted")
	}
	if _, err := parseModule(bytes.NewReader(moduleTarball(t, "x", `{"name":"myorg-hello"}`))); err == nil {
		t.Error("missing version accepted")
	}
}

func tarballOf(t *testing.T, entries ...*tar.Header) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	for _, h := range entries {
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if h.Typeflag == tar.TypeReg {
			body := helloMetadata
			if !strings.HasSuffix(h.Name, "metadata.json") {
				body = strings.Repeat("x", int(h.Size))
			}
			_, _ = tw.Write([]byte(body))
		}
	}
	_ = tw.Close()
	_ = zw.Close()
	return buf.Bytes()
}

func TestParseModuleSkipsNonRegularMetadata(t *testing.T) {
	raw := tarballOf(t,
		&tar.Header{Name: "myorg-hello-0.1.0/metadata.json", Typeflag: tar.TypeSymlink, Linkname: "/etc/passwd"},
		&tar.Header{Name: "myorg-hello-0.1.0/metadata.json/", Typeflag: tar.TypeDir, Mode: 0o755},
		&tar.Header{Name: "myorg-hello-0.1.0/metadata.json", Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(helloMetadata))},
	)
	meta, err := parseModule(bytes.NewReader(raw))
	if err != nil || meta.Slug != "myorg-hello" || meta.Version != "0.1.0" {
		t.Fatalf("got %v, %v", meta, err)
	}
}

func TestParseModuleCapsDecompressedScan(t *testing.T) {
	defer func(n int64) { maxModuleScan = n }(maxModuleScan)
	maxModuleScan = 64 << 10
	raw := tarballOf(t,
		&tar.Header{Name: "myorg-hello-0.1.0/big", Typeflag: tar.TypeReg, Mode: 0o644, Size: 1 << 20},
		&tar.Header{Name: "myorg-hello-0.1.0/metadata.json", Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(helloMetadata))},
	)
	if _, err := parseModule(bytes.NewReader(raw)); err == nil || !strings.Contains(err.Error(), "scan limit") {
		t.Fatalf("expected scan limit error, got %v", err)
	}
	maxModuleScan = 2 << 20
	if _, err := parseModule(bytes.NewReader(raw)); err != nil {
		t.Fatalf("under cap: %v", err)
	}
}

func TestAfterUploadCapsTarballSize(t *testing.T) {
	defer func(n int64) { maxModuleTarball = n }(maxModuleTarball)
	raw := moduleTarball(t, "myorg-hello-0.1.0", helloMetadata)
	maxModuleTarball = int64(len(raw)) - 1
	store := &fakeStore{}
	(&Provider{}).AfterUpload(context.Background(), "forge", "v3/files/myorg-hello-0.1.0.tar.gz", "sha256:abc", fakeBlobs{raw}, store)
	if store.inserted != nil {
		t.Error("oversized tarball indexed")
	}
	maxModuleTarball = int64(len(raw))
	(&Provider{}).AfterUpload(context.Background(), "forge", "v3/files/myorg-hello-0.1.0.tar.gz", "sha256:abc", fakeBlobs{raw}, store)
	if store.inserted == nil {
		t.Error("tarball at cap not indexed")
	}
}

func TestReleaseListClampsPagination(t *testing.T) {
	files := releases("0.1.0", "0.2.0", "0.3.0")
	for _, q := range []string{"offset=9223372036854775807", "offset=9223372036854775807&limit=9223372036854775807", "offset=-5&limit=-5", "limit=9223372036854775807"} {
		code, body := get(t, files, "v3/releases?module=myorg-hello&"+q)
		if code != http.StatusOK {
			t.Fatalf("%s: code %d", q, code)
		}
		pg := body["pagination"].(map[string]any)
		if l := pg["limit"].(float64); l < 1 || l > 100 {
			t.Errorf("%s: limit %v", q, l)
		}
		if o := pg["offset"].(float64); o < 0 || o > 3 {
			t.Errorf("%s: offset %v", q, o)
		}
	}
	_, body := get(t, files, "v3/releases?module=myorg-hello&offset=9223372036854775807")
	if len(body["results"].([]any)) != 0 || body["pagination"].(map[string]any)["next"] != nil {
		t.Errorf("past-end page: %v", body)
	}
}

func get(t *testing.T, files provider.FileStore, target string) (int, map[string]any) {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/x/"+target, nil)
	path, _, _ := strings.Cut(target, "?")
	if !(&Provider{}).ServeLocalIndex(rec, req, files, "forge", path) {
		return 0, nil
	}
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	return rec.Code, body
}

func releases(versions ...string) fakeReader {
	var f fakeReader
	for i, v := range versions {
		f.metas = append(f.metas, provider.PuppetMetadata{
			FilePath: "v3/files/myorg-hello-" + v + ".tar.gz", ContentHash: "sha256:feed", Slug: "myorg-hello",
			Version: v, MetadataJSON: `{"name":"myorg-hello","version":"` + v + `"}`, MD5: "m" + v,
			CreatedAt: time.Unix(int64(i), 0),
		})
	}
	f.metas = append(f.metas, provider.PuppetMetadata{Slug: "other-mod", Version: "9.9.9", MetadataJSON: `{}`})
	return f
}

func TestReleaseListPaginatesNewestFirst(t *testing.T) {
	files := releases("0.1.0", "0.10.0", "0.2.0", "1.0.0-rc.1", "1.0.0")
	code, body := get(t, files, "v3/releases?module=myorg/hello&limit=2")
	if code != http.StatusOK {
		t.Fatalf("code %d", code)
	}
	results := body["results"].([]any)
	pg := body["pagination"].(map[string]any)
	if len(results) != 2 || results[0].(map[string]any)["version"] != "1.0.0" || results[1].(map[string]any)["version"] != "1.0.0-rc.1" {
		t.Fatalf("page 1: %v", results)
	}
	if pg["total"] != float64(5) || pg["next"] != "/v3/releases?limit=2&module=myorg-hello&offset=2" || pg["previous"] != nil {
		t.Errorf("pagination: %v", pg)
	}

	_, body = get(t, files, "v3/releases?module=myorg-hello&limit=2&offset=4")
	results = body["results"].([]any)
	pg = body["pagination"].(map[string]any)
	if len(results) != 1 || results[0].(map[string]any)["version"] != "0.1.0" || pg["next"] != nil || pg["previous"] != "/v3/releases?limit=2&module=myorg-hello&offset=2" {
		t.Errorf("last page: %v %v", results, pg)
	}

	_, body = get(t, files, "v3/releases?module=nobody-none")
	if len(body["results"].([]any)) != 0 {
		t.Error("unknown module returned results")
	}
}

func TestReleaseObject(t *testing.T) {
	code, body := get(t, releases("0.1.0"), "v3/releases/myorg-hello-0.1.0")
	if code != http.StatusOK {
		t.Fatalf("code %d", code)
	}
	if body["file_uri"] != "/v3/files/myorg-hello-0.1.0.tar.gz" || body["file_md5"] != "m0.1.0" || body["file_sha256"] != "feed" {
		t.Errorf("release: %v", body)
	}
	if body["metadata"].(map[string]any)["name"] != "myorg-hello" || body["module"].(map[string]any)["owner"].(map[string]any)["username"] != "myorg" {
		t.Errorf("release refs: %v", body)
	}
	if code, _ := get(t, releases("0.1.0"), "v3/releases/myorg-hello-0.2.0"); code != http.StatusNotFound {
		t.Errorf("missing version: %d", code)
	}
}

func TestModuleObject(t *testing.T) {
	code, body := get(t, releases("0.9.0", "0.10.0"), "v3/modules/myorg-hello")
	if code != http.StatusOK {
		t.Fatalf("code %d", code)
	}
	if body["slug"] != "myorg-hello" || body["name"] != "hello" || body["current_release"].(map[string]any)["version"] != "0.10.0" || len(body["releases"].([]any)) != 2 {
		t.Errorf("module: %v", body)
	}
	if code, _ := get(t, releases(), "v3/modules/myorg-missing"); code != http.StatusNotFound {
		t.Errorf("missing module: %d", code)
	}
}

func TestFilesFallThrough(t *testing.T) {
	if code, _ := get(t, releases(), "v3/files/myorg-hello-0.1.0.tar.gz"); code != 0 {
		t.Error("file downloads must fall through to the stored file")
	}
}

func TestCompareSemver(t *testing.T) {
	ordered := []string{"0.1.0", "0.2.0", "0.10.0", "1.0.0-alpha", "1.0.0-alpha.1", "1.0.0-beta", "1.0.0-beta.2", "1.0.0-beta.11", "1.0.0", "1.0.1+build"}
	for i := 1; i < len(ordered); i++ {
		if CompareSemver(ordered[i-1], ordered[i]) >= 0 || CompareSemver(ordered[i], ordered[i-1]) <= 0 {
			t.Errorf("%s should sort before %s", ordered[i-1], ordered[i])
		}
	}
	if CompareSemver("1.0.0+a", "1.0.0+b") != 0 {
		t.Error("build metadata must not affect precedence")
	}
}
