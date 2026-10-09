package virtual

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/klauspost/compress/zstd"
	"github.com/ulikunitz/xz"
)

func primaryXML(pkgs ...string) []byte {
	return []byte(`<?xml version="1.0" encoding="UTF-8"?>
<metadata xmlns="http://linux.duke.edu/metadata/common" xmlns:rpm="http://linux.duke.edu/metadata/rpm" packages="` + fmt.Sprint(len(pkgs)) + `">
` + strings.Join(pkgs, "\n") + `
</metadata>`)
}

func primaryPkgXML(name, ver, pkgid, href string) string {
	return `<package type="rpm">
  <name>` + name + `</name>
  <arch>x86_64</arch>
  <version epoch="0" ver="` + ver + `" rel="1"/>
  <checksum type="sha256" pkgid="YES">` + pkgid + `</checksum>
  <location href="` + href + `"/>
  <format>
    <rpm:provides><rpm:entry name="` + name + `"/></rpm:provides>
  </format>
</package>`
}

func filelistsXML(pkgids ...string) []byte {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0"?><filelists xmlns="http://linux.duke.edu/metadata/filelists" packages="1">`)
	for _, id := range pkgids {
		b.WriteString(`<package pkgid="` + id + `" name="x" arch="x86_64"><version epoch="0" ver="1" rel="1"/><file>/usr/bin/` + id + `</file></package>`)
	}
	b.WriteString(`</filelists>`)
	return []byte(b.String())
}

func gunzip(t *testing.T, b []byte) []byte {
	t.Helper()
	r, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func mergedFile(t *testing.T, repo *RPMRepo, dtype string) []byte {
	t.Helper()
	for href, body := range repo.Files {
		if strings.HasSuffix(href, "-"+dtype+".xml.gz") {
			return gunzip(t, body)
		}
	}
	t.Fatalf("no %s file in merged repo", dtype)
	return nil
}

func TestMergeRPMDedupePriority(t *testing.T) {
	members := []RPMMember{
		{RemoteName: "first", Timestamp: 100, Data: map[string][]byte{
			"primary":   primaryXML(primaryPkgXML("foo", "1.0", "aaa", "Packages/foo-1.0.rpm")),
			"filelists": filelistsXML("aaa"),
		}},
		{RemoteName: "second", Timestamp: 200, Data: map[string][]byte{
			"primary": primaryXML(
				primaryPkgXML("foo", "1.0", "bbb", "Packages/foo-1.0-rebuilt.rpm"),
				primaryPkgXML("bar", "2.0", "ccc", "Packages/bar-2.0.rpm"),
			),
			"filelists": filelistsXML("bbb", "ccc"),
		}},
	}
	repo, err := MergeRPM(members)
	if err != nil {
		t.Fatal(err)
	}

	primary := string(mergedFile(t, repo, "primary"))
	if strings.Count(primary, "<name>foo</name>") != 1 {
		t.Fatalf("duplicate NEVRA not deduped:\n%s", primary)
	}
	if !strings.Contains(primary, `href="first/Packages/foo-1.0.rpm"`) || strings.Contains(primary, "foo-1.0-rebuilt") {
		t.Fatalf("first member should win duplicate NEVRA:\n%s", primary)
	}
	if !strings.Contains(primary, `href="second/Packages/bar-2.0.rpm"`) {
		t.Fatalf("unique package from second member missing:\n%s", primary)
	}
	if !strings.Contains(primary, `packages="2"`) {
		t.Fatalf("package count wrong:\n%s", primary)
	}

	filelists := string(mergedFile(t, repo, "filelists"))
	if !strings.Contains(filelists, "/usr/bin/aaa") || !strings.Contains(filelists, "/usr/bin/ccc") || strings.Contains(filelists, "/usr/bin/bbb") {
		t.Fatalf("filelists must follow the winning primary entries:\n%s", filelists)
	}

	other := string(mergedFile(t, repo, "other"))
	if !strings.Contains(other, `packages="0"`) {
		t.Fatalf("members without other data should yield an empty other.xml:\n%s", other)
	}

	if !strings.Contains(string(repo.Repomd), "<revision>200</revision>") {
		t.Fatalf("revision should be the newest member timestamp:\n%s", repo.Repomd)
	}
}

func TestMergeRPMOutputIsWellFormed(t *testing.T) {
	repo, err := MergeRPM([]RPMMember{{RemoteName: "a", Data: map[string][]byte{
		"primary": primaryXML(primaryPkgXML("foo", "1.0", "aaa", "Packages/foo.rpm")),
	}}})
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		XMLName  xml.Name
		Packages []struct {
			Name     string `xml:"name"`
			Provides []struct {
				Name string `xml:"name,attr"`
			} `xml:"format>provides>entry"`
		} `xml:"package"`
	}
	if err := xml.Unmarshal(mergedFile(t, repo, "primary"), &doc); err != nil {
		t.Fatal(err)
	}
	if doc.XMLName.Space != "http://linux.duke.edu/metadata/common" || len(doc.Packages) != 1 {
		t.Fatalf("unexpected merged primary: %+v", doc)
	}
	if len(doc.Packages[0].Provides) != 1 || doc.Packages[0].Provides[0].Name != "foo" {
		t.Fatalf("rpm: namespaced format lost: %+v", doc.Packages[0])
	}
}

func TestMergeRPMHrefRewriting(t *testing.T) {
	based := func(name, base string) string {
		return `<package type="rpm"><name>` + name + `</name><arch>noarch</arch><version epoch="0" ver="1" rel="1"/>` +
			`<checksum type="sha256" pkgid="YES">` + name + `</checksum><location xml:base="` + base + `" href="Packages/` + name + `.rpm"/></package>`
	}
	repo, err := MergeRPM([]RPMMember{{RemoteName: "gh", Bases: []string{"https://up.example/el9", "https://mirror.example/el9/"}, Data: map[string][]byte{
		"primary": primaryXML(
			primaryPkgXML("foo", "1.0", "aaa", "storytold/photocraft/releases/download/v1.0/foo&amp;bar.rpm"),
			primaryPkgXML("lead", "1.0", "bbb", "/Packages/lead.rpm"),
			based("root", "https://up.example/el9/"),
			based("sub", "https://mirror.example/el9/extra"),
			based("ext", "https://other.example/"),
		),
	}}})
	if err != nil {
		t.Fatal(err)
	}
	primary := string(mergedFile(t, repo, "primary"))
	for _, want := range []string{
		`<location href="gh/storytold/photocraft/releases/download/v1.0/foo&amp;bar.rpm"/>`,
		`<location href="gh/Packages/lead.rpm"/>`,
		`<location href="gh/Packages/root.rpm"/>`,
		`<location href="gh/extra/Packages/sub.rpm"/>`,
		`<location xml:base="https://other.example/" href="Packages/ext.rpm"/>`,
	} {
		if !strings.Contains(primary, want) {
			t.Errorf("missing %s in:\n%s", want, primary)
		}
	}
}

func TestMergeRPMChecksums(t *testing.T) {
	repo, err := MergeRPM([]RPMMember{{RemoteName: "a", Timestamp: 42, Data: map[string][]byte{
		"primary": primaryXML(primaryPkgXML("foo", "1.0", "aaa", "Packages/foo.rpm")),
	}}})
	if err != nil {
		t.Fatal(err)
	}

	var md struct {
		Data []struct {
			Type         string `xml:"type,attr"`
			Checksum     string `xml:"checksum"`
			OpenChecksum string `xml:"open-checksum"`
			Location     struct {
				Href string `xml:"href,attr"`
			} `xml:"location"`
			Timestamp int64 `xml:"timestamp"`
			Size      int   `xml:"size"`
			OpenSize  int   `xml:"open-size"`
		} `xml:"data"`
	}
	if err := xml.Unmarshal(repo.Repomd, &md); err != nil {
		t.Fatal(err)
	}
	if len(md.Data) != 3 {
		t.Fatalf("want primary/filelists/other, got %d entries", len(md.Data))
	}
	for _, d := range md.Data {
		body, ok := repo.Files[d.Location.Href]
		if !ok {
			t.Fatalf("%s: location %q not served", d.Type, d.Location.Href)
		}
		sum := sha256.Sum256(body)
		if hex.EncodeToString(sum[:]) != d.Checksum || len(body) != d.Size {
			t.Errorf("%s: checksum/size do not match served bytes", d.Type)
		}
		open := gunzip(t, body)
		osum := sha256.Sum256(open)
		if hex.EncodeToString(osum[:]) != d.OpenChecksum || len(open) != d.OpenSize {
			t.Errorf("%s: open-checksum/open-size do not match decompressed bytes", d.Type)
		}
		if d.Timestamp != 42 {
			t.Errorf("%s: timestamp %d, want 42", d.Type, d.Timestamp)
		}
	}

	again, _ := MergeRPM([]RPMMember{{RemoteName: "a", Timestamp: 42, Data: map[string][]byte{
		"primary": primaryXML(primaryPkgXML("foo", "1.0", "aaa", "Packages/foo.rpm")),
	}}})
	if !bytes.Equal(repo.Repomd, again.Repomd) {
		t.Error("merge must be deterministic so repomd checksums stay valid across requests")
	}
}

func TestParseRepomd(t *testing.T) {
	locs, ts, err := parseRepomd([]byte(`<repomd xmlns="http://linux.duke.edu/metadata/repo">
  <data type="primary"><location href="repodata/p-primary.xml.zst"/><timestamp>10</timestamp></data>
  <data type="primary_db"><location href="repodata/p.sqlite.bz2"/><timestamp>99</timestamp></data>
  <data type="other"><location href="repodata/o-other.xml.gz"/><timestamp>20</timestamp></data>
</repomd>`))
	if err != nil {
		t.Fatal(err)
	}
	if locs["primary"] != "repodata/p-primary.xml.zst" || locs["other"] != "repodata/o-other.xml.gz" || len(locs) != 2 {
		t.Fatalf("unexpected locations: %v", locs)
	}
	if ts != 20 {
		t.Fatalf("timestamp %d, want 20 (sqlite entries ignored)", ts)
	}
	if _, _, err := parseRepomd([]byte(`<repomd><data type="other"><location href="x"/></data></repomd>`)); err == nil {
		t.Fatal("repomd without primary should error")
	}
}

func TestDecompress(t *testing.T) {
	plain := []byte("<metadata/>")

	var xzBuf bytes.Buffer
	xw, _ := xz.NewWriter(&xzBuf)
	_, _ = xw.Write(plain)
	_ = xw.Close()

	zw, _ := zstd.NewWriter(nil)
	zst := zw.EncodeAll(plain, nil)

	for href, body := range map[string][]byte{
		"p.xml":     plain,
		"p.xml.gz":  gzipDeterministic(plain),
		"p.xml.xz":  xzBuf.Bytes(),
		"p.xml.zst": zst,
	} {
		got, err := decompress(href, body)
		if err != nil || !bytes.Equal(got, plain) {
			t.Errorf("%s: got %q, %v", href, got, err)
		}
	}
}
