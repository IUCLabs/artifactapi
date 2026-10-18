package ansible

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"testing"
)

type member struct {
	hdr  tar.Header
	body string
}

func collectionTar(t *testing.T, members ...member) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	for _, m := range members {
		h := m.hdr
		if h.Typeflag == 0 {
			h.Typeflag = tar.TypeReg
		}
		if h.Typeflag == tar.TypeReg {
			h.Size = int64(len(m.body))
		}
		h.Mode = 0o644
		if err := tw.WriteHeader(&h); err != nil {
			t.Fatal(err)
		}
		_, _ = tw.Write([]byte(m.body))
	}
	_ = tw.Close()
	_ = zw.Close()
	return buf.Bytes()
}

func manifest(ns, name, version, deps string) member {
	return member{tar.Header{Name: "MANIFEST.json"}, `{"collection_info":{"namespace":"` + ns + `","name":"` + name +
		`","version":"` + version + `","authors":["a"],"dependencies":` + deps + `}}`}
}

func TestParseCollection(t *testing.T) {
	e, err := ParseCollection(bytes.NewReader(collectionTar(t,
		member{tar.Header{Name: "plugins/", Typeflag: tar.TypeDir}, ""},
		member{tar.Header{Name: "plugins/modules/hello.py"}, "x"},
		member{tar.Header{Name: "docs/link", Typeflag: tar.TypeSymlink, Linkname: "../plugins/modules/hello.py"}, ""},
		manifest("myorg", "e2e_hello", "1.0.0-rc.1+b1", `{"ansible.posix":">=1.0.0"}`),
	)))
	if err != nil {
		t.Fatal(err)
	}
	if e.Namespace != "myorg" || e.Name != "e2e_hello" || e.Version != "1.0.0-rc.1+b1" || e.Filename() != "myorg-e2e_hello-1.0.0-rc.1+b1.tar.gz" {
		t.Fatalf("entry = %+v", e)
	}
	var md struct {
		Authors      []string          `json:"authors"`
		Dependencies map[string]string `json:"dependencies"`
	}
	if err := json.Unmarshal(e.Metadata, &md); err != nil || md.Dependencies["ansible.posix"] != ">=1.0.0" || len(md.Authors) != 1 {
		t.Fatalf("metadata = %s (%v)", e.Metadata, err)
	}

	e, err = ParseCollection(bytes.NewReader(collectionTar(t, manifest("a", "b", "1.0.0", "null"))))
	if err != nil || !strings.Contains(string(e.Metadata), `"dependencies":{}`) {
		t.Fatalf("null dependencies = %s (%v), want {}", e.Metadata, err)
	}
}

func TestParseCollectionRejects(t *testing.T) {
	ok := manifest("a", "b", "1.0.0", "{}")
	tests := []struct {
		name    string
		members []member
		want    string
	}{
		{"no manifest", []member{{tar.Header{Name: "README.md"}, "x"}}, "no MANIFEST.json"},
		{"nested manifest only", []member{{tar.Header{Name: "x/MANIFEST.json"}, ok.body}}, "no MANIFEST.json"},
		{"dotdot", []member{ok, {tar.Header{Name: "../evil"}, "x"}}, "escapes"},
		{"dotdot after clean", []member{ok, {tar.Header{Name: "a/../../evil"}, "x"}}, "escapes"},
		{"absolute", []member{ok, {tar.Header{Name: "/etc/passwd"}, "x"}}, "escapes"},
		{"symlink escape", []member{ok, {tar.Header{Name: "a/l", Typeflag: tar.TypeSymlink, Linkname: "../../x"}, ""}}, "symlink"},
		{"absolute symlink", []member{ok, {tar.Header{Name: "l", Typeflag: tar.TypeSymlink, Linkname: "/etc"}, ""}}, "symlink"},
		{"hardlink", []member{ok, {tar.Header{Name: "l", Typeflag: tar.TypeLink, Linkname: "MANIFEST.json"}, ""}}, "unsupported type"},
		{"duplicate manifest", []member{ok, ok}, "duplicate"},
		{"bad namespace", []member{manifest("Bad", "b", "1.0.0", "{}")}, "invalid collection"},
		{"bad version", []member{manifest("a", "b", "1.0", "{}")}, "invalid collection"},
		{"bad dependency", []member{manifest("a", "b", "1.0.0", `{"nodot":"*"}`)}, "invalid dependency"},
		{"list dependencies", []member{manifest("a", "b", "1.0.0", `["x.y"]`)}, "dependencies must map"},
		{"huge manifest", []member{{tar.Header{Name: "MANIFEST.json"}, strings.Repeat(" ", int(MaxManifestBytes)+1)}}, "exceeds"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ParseCollection(bytes.NewReader(collectionTar(t, tt.members...)))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want %q", err, tt.want)
			}
		})
	}
	if _, err := ParseCollection(strings.NewReader("not gzip")); err == nil {
		t.Error("non-gzip input accepted")
	}
}

func TestParseCollectionScanLimit(t *testing.T) {
	old := MaxScanBytes
	MaxScanBytes = 4096
	defer func() { MaxScanBytes = old }()
	_, err := ParseCollection(bytes.NewReader(collectionTar(t,
		member{tar.Header{Name: "big"}, strings.Repeat("x", 8192)},
		manifest("a", "b", "1.0.0", "{}"),
	)))
	if err == nil || !strings.Contains(err.Error(), "decompressed size limit") {
		t.Fatalf("err = %v, want decompressed size limit", err)
	}
}

func TestParseAPIPath(t *testing.T) {
	tests := []struct {
		in   string
		want APIPath
	}{
		{"api", APIPath{Kind: APIRoot}},
		{"api/", APIPath{Kind: APIRoot}},
		{"api/v3/collections/myorg/e2e_hello/", APIPath{Kind: APICollection, Namespace: "myorg", Name: "e2e_hello"}},
		{"api/v3/collections/myorg/e2e_hello/versions", APIPath{Kind: APIVersions, Namespace: "myorg", Name: "e2e_hello"}},
		{"api/v3/collections/myorg/e2e_hello/versions/1.0.0/", APIPath{Kind: APIVersion, Namespace: "myorg", Name: "e2e_hello", Version: "1.0.0"}},
		{"api/v3/imports/collections/myorg-e2e_hello-1.0.0/", APIPath{Kind: APIImport, TaskID: "myorg-e2e_hello-1.0.0"}},
		{"api/v3/collections/myorg/e2e_hello/versions/1.0/", APIPath{}},
		{"api/v3/collections/../x/", APIPath{}},
		{"api/v3/collections/myorg/e2e_hello/versions/1.0.0/extra/", APIPath{}},
		{"artifacts/myorg-e2e_hello-1.0.0.tar.gz", APIPath{}},
		{"api/v3/imports/collections/a/b/", APIPath{}},
	}
	for _, tt := range tests {
		if got := ParseAPIPath(tt.in); got != tt.want {
			t.Errorf("ParseAPIPath(%q) = %+v, want %+v", tt.in, got, tt.want)
		}
	}
}

func TestVersionsPage(t *testing.T) {
	refs := []VersionRef{{Version: "3.0.0"}, {Version: "2.0.0"}, {Version: "1.0.0"}}
	list := "/r/api/v3/collections/a/b/versions/"
	links := func(p map[string]any) map[string]any { return p["links"].(map[string]any) }
	versions := func(p map[string]any) (out []string) {
		for _, d := range p["data"].([]map[string]any) {
			out = append(out, d["version"].(string))
		}
		return out
	}

	p := VersionsPage("https://h/r", "a", "b", refs, 2, 0)
	if got := versions(p); strings.Join(got, ",") != "3.0.0,2.0.0" {
		t.Fatalf("page 1 = %v", got)
	}
	if l := links(p); l["previous"] != nil || l["next"] != list+"?limit=2&offset=2" || l["last"] != list+"?limit=2&offset=2" {
		t.Fatalf("page 1 links = %v", l)
	}
	if p["meta"].(map[string]any)["count"] != 3 {
		t.Fatalf("count = %v", p["meta"])
	}

	p = VersionsPage("https://h/r", "a", "b", refs, 2, 2)
	if got := versions(p); strings.Join(got, ",") != "1.0.0" {
		t.Fatalf("page 2 = %v", got)
	}
	if l := links(p); l["next"] != nil || l["previous"] != list+"?limit=2&offset=0" {
		t.Fatalf("page 2 links = %v", l)
	}

	if got := versions(VersionsPage("", "a", "b", refs, 2, 99)); len(got) != 0 {
		t.Fatalf("offset past end = %v, want empty", got)
	}
}

// TestVersionsPageFollow pages through more than 100 versions the way
// ansible-galaxy does: old clients splice next onto the versions URL in place
// of its path, new ones urljoin it onto the server.
func TestVersionsPageFollow(t *testing.T) {
	var refs []VersionRef
	for i := 104; i >= 0; i-- {
		refs = append(refs, VersionRef{Version: fmt.Sprintf("1.0.%d", i)})
	}
	server := "https://h:8443/api/v1/local/r%2Bx/"
	base := strings.TrimSuffix(server, "/")
	versionsURL := server + "api/v3/collections/a/b/versions/"
	for _, follow := range map[string]func(string) string{
		"replace": func(link string) string {
			u, _ := url.Parse(versionsURL)
			return strings.Replace(versionsURL, u.EscapedPath(), link, 1)
		},
		"urljoin": func(link string) string {
			s, _ := url.Parse(server)
			l, _ := url.Parse(link)
			return s.ResolveReference(l).String()
		},
	} {
		var got []string
		next := versionsURL + "?limit=100"
		for pages := 0; next != ""; pages++ {
			if pages > 3 {
				t.Fatal("pagination does not terminate")
			}
			u, err := url.Parse(next)
			if err != nil || u.Host != "h:8443" || !strings.HasPrefix(u.EscapedPath(), "/api/v1/local/r%2Bx/api/v3/") {
				t.Fatalf("followed link %q is not on the server", next)
			}
			limit, offset := Page(u.Query())
			p := VersionsPage(base, "a", "b", refs, limit, offset)
			for _, d := range p["data"].([]map[string]any) {
				got = append(got, d["version"].(string))
			}
			next = ""
			if l, ok := p["links"].(map[string]any)["next"].(string); ok {
				next = follow(l)
			}
		}
		if len(got) != 105 || got[0] != "1.0.104" || got[104] != "1.0.0" {
			t.Fatalf("collected %d versions, first %v", len(got), got[:1])
		}
	}
}

func TestCollectionDocTimes(t *testing.T) {
	d := CollectionDoc("", "a", "b", []VersionRef{
		{CreatedAt: "2026-01-01T00:00:05Z", UpdatedAt: "2026-01-01T00:00:05Z"},
		{CreatedAt: "2026-01-01T00:00:05.5Z", UpdatedAt: "2026-01-01T00:00:05.5Z"},
		{CreatedAt: "2026-01-01T00:00:04.9Z", UpdatedAt: "2026-01-01T00:00:04.9Z"},
	})
	if d["created_at"] != "2026-01-01T00:00:04.9Z" || d["updated_at"] != "2026-01-01T00:00:05.5Z" {
		t.Fatalf("created/updated = %v/%v", d["created_at"], d["updated_at"])
	}
}

func TestPage(t *testing.T) {
	for _, tt := range []struct {
		q             string
		limit, offset int
	}{
		{"", 100, 0},
		{"limit=5&offset=10", 5, 10},
		{"limit=5000", 100, 0},
		{"limit=0&offset=-1", 100, 0},
	} {
		q, _ := url.ParseQuery(tt.q)
		if l, o := Page(q); l != tt.limit || o != tt.offset {
			t.Errorf("Page(%q) = %d,%d want %d,%d", tt.q, l, o, tt.limit, tt.offset)
		}
	}
}
