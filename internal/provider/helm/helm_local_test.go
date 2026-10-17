package helm

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/IUCLabs/artifactapi/internal/provider"
)

func chartTgz(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	for name, body := range files {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(body))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

type fakeBlobs struct{ data []byte }

func (f fakeBlobs) Download(context.Context, string) (io.ReadCloser, int64, error) {
	return io.NopCloser(bytes.NewReader(f.data)), int64(len(f.data)), nil
}

type fakeStore struct {
	inserted *provider.HelmMetadata
	metas    []provider.HelmMetadata
}

func (f *fakeStore) InsertRPMMetadata(context.Context, *provider.RPMMetadata) error { return nil }
func (f *fakeStore) InsertDebMetadata(context.Context, *provider.DebMetadata) error { return nil }
func (f *fakeStore) InsertHelmMetadata(_ context.Context, m *provider.HelmMetadata) error {
	f.inserted = m
	return nil
}
func (f *fakeStore) ListHelmMetadataEntries(context.Context, string) ([]provider.HelmMetadata, error) {
	return f.metas, nil
}
func (f *fakeStore) ListFilesByPrefix(context.Context, string, string) ([]provider.FileEntry, error) {
	return nil, nil
}
func (f *fakeStore) ListPackages(context.Context, string) ([]string, error) { return nil, nil }

func TestValidateUpload(t *testing.T) {
	p := &Provider{}
	for in, want := range map[string]string{
		"mychart-0.1.0.tgz":        "mychart-0.1.0.tgz",
		"charts/mychart-0.1.0.tgz": "mychart-0.1.0.tgz",
		"../../mychart-0.1.0.tgz":  "mychart-0.1.0.tgz",
	} {
		sp, ct, err := p.ValidateUpload(in)
		if err != nil || sp != want || ct != "application/gzip" {
			t.Errorf("ValidateUpload(%q) = %q %q %v", in, sp, ct, err)
		}
	}
	for _, bad := range []string{"index.yaml", "chart.tar.gz", ".tgz", ""} {
		if _, _, err := p.ValidateUpload(bad); err == nil {
			t.Errorf("ValidateUpload(%q) accepted", bad)
		}
	}
}

func TestAfterUploadParsesTopLevelChart(t *testing.T) {
	data := chartTgz(t, map[string]string{
		"mychart/charts/sub/Chart.yaml": "apiVersion: v2\nname: sub\nversion: 9.9.9\n",
		"mychart/Chart.yaml":            "apiVersion: v2\nname: mychart\nversion: 1.0\ndescription: hi\n",
		"mychart/values.yaml":           "a: 1\n",
	})
	store := &fakeStore{}
	(&Provider{}).AfterUpload(context.Background(), "r", "mychart-1.0.tgz", "sha256:ab", fakeBlobs{data}, store)
	m := store.inserted
	if m == nil || m.Name != "mychart" || m.Version != "1.0" || m.FilePath != "mychart-1.0.tgz" || m.ContentHash != "sha256:ab" {
		t.Fatalf("inserted %+v", m)
	}
}

func TestAfterUploadRejectsChartWithoutMetadata(t *testing.T) {
	store := &fakeStore{}
	data := chartTgz(t, map[string]string{"mychart/Chart.yaml": "apiVersion: v2\nname: mychart\n"})
	(&Provider{}).AfterUpload(context.Background(), "r", "x.tgz", "sha256:ab", fakeBlobs{data}, store)
	if store.inserted != nil {
		t.Fatal("chart without version was recorded")
	}
	(&Provider{}).AfterUpload(context.Background(), "r", "x.tgz", "sha256:ab", fakeBlobs{[]byte("not gzip")}, store)
	if store.inserted != nil {
		t.Fatal("non-gzip upload was recorded")
	}
}

func TestServeLocalIndex(t *testing.T) {
	t1 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	t2 := t1.Add(time.Hour)
	store := &fakeStore{metas: []provider.HelmMetadata{
		{FilePath: "mychart-1.0.tgz", ContentHash: "sha256:aa", Name: "mychart", Version: "1.0", CreatedAt: t1,
			ChartYAML: "apiVersion: v2\nname: mychart\nversion: 1.0\nappVersion: \"2\"\ndescription: hi\n"},
		{FilePath: "other-0.2.0.tgz", ContentHash: "sha256:bb", Name: "other", Version: "0.2.0", CreatedAt: t2,
			ChartYAML: "apiVersion: v2\nname: other\nversion: 0.2.0\n"},
	}}
	p := &Provider{}

	if p.ServeLocalIndex(httptest.NewRecorder(), httptest.NewRequest("GET", "/", nil), store, "r", "mychart-1.0.tgz") {
		t.Fatal("chart path handled as index")
	}
	rec := httptest.NewRecorder()
	if !p.ServeLocalIndex(rec, httptest.NewRequest("GET", "/", nil), store, "r", "./index.yaml") {
		t.Fatal("index.yaml not handled")
	}
	if rec.Code != 200 || rec.Header().Get("Content-Type") != "text/yaml" {
		t.Fatalf("code=%d ct=%q", rec.Code, rec.Header().Get("Content-Type"))
	}

	var idx struct {
		APIVersion string `yaml:"apiVersion"`
		Generated  string `yaml:"generated"`
		Entries    map[string][]map[string]any
	}
	if err := yaml.Unmarshal(rec.Body.Bytes(), &idx); err != nil {
		t.Fatal(err)
	}
	if idx.APIVersion != "v1" || idx.Generated != t2.Format(time.RFC3339Nano) {
		t.Errorf("header %+v", idx)
	}
	e := idx.Entries["mychart"][0]
	if e["digest"] != "aa" || e["description"] != "hi" || e["appVersion"] != "2" || e["apiVersion"] != "v2" {
		t.Errorf("entry %v", e)
	}
	if urls, _ := e["urls"].([]any); len(urls) != 1 || urls[0] != "mychart-1.0.tgz" {
		t.Errorf("urls %v", e["urls"])
	}
	if e["version"] != "1.0" || e["name"] != "mychart" {
		t.Errorf("version must stay a string: %#v", e["version"])
	}
	if len(idx.Entries["other"]) != 1 {
		t.Errorf("other entries %v", idx.Entries["other"])
	}

	again, err := p.GenerateLocalIndex(context.Background(), store, "r", "index.yaml")
	if err != nil || !bytes.Equal(again, rec.Body.Bytes()) {
		t.Errorf("index not deterministic: %v", err)
	}
}

func TestAfterUploadRejectsFilenameMismatch(t *testing.T) {
	store := &fakeStore{}
	data := chartTgz(t, map[string]string{"mychart/Chart.yaml": "apiVersion: v2\nname: mychart\nversion: 1.0.0\n"})
	(&Provider{}).AfterUpload(context.Background(), "r", "other-1.0.0.tgz", "sha256:ab", fakeBlobs{data}, store)
	if store.inserted != nil {
		t.Fatal("chart indexed under a filename that does not match name-version")
	}
}

func TestParseChartLimits(t *testing.T) {
	chart := "apiVersion: v2\nname: c\nversion: 1.0.0\n"
	for name, tc := range map[string]struct {
		files map[string]string
		want  string
	}{
		"decompressed": {map[string]string{"c/a.bin": strings.Repeat("\x00", maxChartBytes+1)}, "decompressed bytes"},
		"chart yaml":   {map[string]string{"c/Chart.yaml": chart + "#" + strings.Repeat("x", maxChartYAML)}, "Chart.yaml: exceeds"},
	} {
		_, err := parseChart(bytes.NewReader(chartTgz(t, tc.files)))
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want %q", name, err, tc.want)
		}
	}

	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	for i := 0; i <= maxChartEntries; i++ {
		_ = tw.WriteHeader(&tar.Header{Name: "c/templates/" + strconv.Itoa(i), Mode: 0o644})
	}
	_ = tw.WriteHeader(&tar.Header{Name: "c/Chart.yaml", Mode: 0o644, Size: int64(len(chart))})
	_, _ = tw.Write([]byte(chart))
	_ = tw.Close()
	_ = zw.Close()
	if _, err := parseChart(&buf); err == nil || !strings.Contains(err.Error(), "entries") {
		t.Errorf("entries: err = %v", err)
	}

	if m, err := parseChart(bytes.NewReader(chartTgz(t, map[string]string{"c/Chart.yaml": chart}))); err != nil || m.Name != "c" {
		t.Errorf("valid chart: %v %v", m, err)
	}
}

func TestIndexEscapesURLs(t *testing.T) {
	out, err := generateIndex([]provider.HelmMetadata{{FilePath: "my chart-1.0.0.tgz", Name: "my chart", Version: "1.0.0", ChartYAML: "name: x\n"}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "my%20chart-1.0.0.tgz") {
		t.Errorf("url not escaped:\n%s", out)
	}
}

func TestIndexExpandsAliasesBeforeOverride(t *testing.T) {
	body, err := generateIndex([]provider.HelmMetadata{{
		FilePath: "app-1.0.0.tgz", ContentHash: "sha256:aa", Name: "app", Version: "1.0.0",
		ChartYAML: "apiVersion: v2\nname: app\nversion: &v 1.0.0\nappVersion: *v\n",
	}})
	if err != nil {
		t.Fatal(err)
	}
	var idx struct {
		Entries map[string][]map[string]any
	}
	if err := yaml.Unmarshal(body, &idx); err != nil {
		t.Fatalf("index does not parse: %v\n%s", err, body)
	}
	if e := idx.Entries["app"]; len(e) != 1 || e[0]["appVersion"] != "1.0.0" {
		t.Errorf("entries %v", idx.Entries)
	}
}
