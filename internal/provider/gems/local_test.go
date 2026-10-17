package gems

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"compress/zlib"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"strings"
	"testing"
	"time"
)

const testMetadata = `--- !ruby/object:Gem::Specification
name: myorg-e2e-hello
version: !ruby/object:Gem::Version
  version: 0.1.0
platform: %PLATFORM%
authors:
- e2e
date: 2026-10-17 00:00:00.000000000 Z
dependencies:
- !ruby/object:Gem::Dependency
  name: rack
  requirement: &1 !ruby/object:Gem::Requirement
    requirements:
    - - ">="
      - !ruby/object:Gem::Version
        version: '2.0'
    - - "<"
      - !ruby/object:Gem::Version
        version: '4'
  type: :runtime
  prerelease: false
  version_requirements: *1
- !ruby/object:Gem::Dependency
  name: json
  version_requirements: !ruby/object:Gem::Requirement
    requirements:
    - - "~>"
      - !ruby/object:Gem::Version
        version: '2.1'
  type: :runtime
  prerelease: false
- !ruby/object:Gem::Dependency
  name: rake
  requirement: !ruby/object:Gem::Requirement
    requirements:
    - - ">="
      - !ruby/object:Gem::Version
        version: '0'
  type: :development
  prerelease: false
email:
licenses:
- MIT
metadata:
  source_code_uri: https://example.com
required_ruby_version: !ruby/object:Gem::Requirement
  requirements:
  - - ">="
    - !ruby/object:Gem::Version
      version: '2.7'
required_rubygems_version: !ruby/object:Gem::Requirement
  requirements:
  - - ">="
    - !ruby/object:Gem::Version
      version: '0'
rubygems_version: 3.5.22
specification_version: 4
summary: hi
`

func buildGem(t *testing.T, metadata []byte) []byte {
	t.Helper()
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	_, _ = zw.Write(metadata)
	_ = zw.Close()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for name, body := range map[string][]byte{"data.tar.gz": []byte("x"), "metadata.gz": gz.Bytes()} {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o444, Size: int64(len(body))}); err != nil {
			t.Fatal(err)
		}
		_, _ = tw.Write(body)
	}
	_ = tw.Close()
	return buf.Bytes()
}

func testGem(t *testing.T, platform string) []byte {
	return buildGem(t, []byte(strings.Replace(testMetadata, "%PLATFORM%", platform, 1)))
}

func TestParseGemAndInfoLine(t *testing.T) {
	gem := testGem(t, "ruby")
	s, err := ParseGem(gem)
	if err != nil {
		t.Fatal(err)
	}
	if s.FullName() != "myorg-e2e-hello-0.1.0" || !s.Date.Equal(time.Date(2026, 10, 17, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("spec = %+v", s)
	}
	if len(s.Dependencies) != 3 || s.Dependencies[1].Requirement.String() != "~> 2.1" {
		t.Fatalf("deps = %+v", s.Dependencies)
	}
	sum := sha256.Sum256(gem)
	want := "0.1.0 rack:>= 2.0&< 4,json:~> 2.1|checksum:" + hex.EncodeToString(sum[:]) + ",ruby:>= 2.7"
	if got := InfoLine(s, gem); got != want {
		t.Fatalf("InfoLine =\n %s\nwant\n %s", got, want)
	}

	plat, err := ParseGem(testGem(t, "x86_64-linux"))
	if err != nil {
		t.Fatal(err)
	}
	if plat.FullName() != "myorg-e2e-hello-0.1.0-x86_64-linux" || InfoKey(InfoLine(plat, nil)) != "0.1.0-x86_64-linux" {
		t.Fatalf("platform gem: %s / %s", plat.FullName(), InfoLine(plat, nil))
	}
}

func TestParseGemRejects(t *testing.T) {
	big := []byte(testMetadata + "description: " + strings.Repeat("a", maxMetadataBytes) + "\n")
	cases := map[string][]byte{
		"not a tar":      []byte("garbage"),
		"no metadata":    func() []byte { var b bytes.Buffer; tw := tar.NewWriter(&b); _ = tw.Close(); return b.Bytes() }(),
		"bad name":       buildGem(t, []byte(strings.Replace(testMetadata, "name: myorg-e2e-hello", "name: ../x", 1))),
		"bad version":    buildGem(t, []byte(strings.Replace(testMetadata, "version: 0.1.0", "version: 'a b'", 1))),
		"bad dep op":     buildGem(t, []byte(strings.Replace(testMetadata, `- - "~>"`, `- - "=~"`, 1))),
		"metadata bomb":  buildGem(t, big),
		"bad platform":   buildGem(t, []byte(strings.Replace(testMetadata, "%PLATFORM%", "x/y", 1))),
		"bad dependency": buildGem(t, []byte(strings.Replace(testMetadata, "name: json", "name: 'a|b'", 1))),
	}
	for name, gem := range cases {
		if _, err := ParseGem(gem); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestVersionsLineMD5(t *testing.T) {
	info := InfoBody([]string{"0.1.0 |checksum:aa", "0.2.0-java |checksum:bb"})
	if string(info) != "---\n0.1.0 |checksum:aa\n0.2.0-java |checksum:bb\n" {
		t.Fatalf("InfoBody = %q", info)
	}
	if got := VersionsLine("g", info); got != "g 0.1.0,0.2.0-java "+MD5Hex(info) {
		t.Fatalf("VersionsLine = %q", got)
	}
}

// Byte layouts below are taken from Ruby 3.3's Marshal.dump.
func TestRubyMarshalEncoding(t *testing.T) {
	for n, want := range map[int]string{0: "\x00", 4: "\x09", 122: "\x7f", 123: "\x01\x7b", 255: "\x01\xff", 256: "\x02\x00\x01", 65536: "\x03\x00\x00\x01", -1: "\xfa", -123: "\x80"} {
		var m rbMarshal
		m.fixnum(n)
		if m.String() != want {
			t.Errorf("fixnum(%d) = %x, want %x", n, m.String(), want)
		}
	}
	var m rbMarshal
	m.str("3.5.22")
	if m.String() != "I\"\x0b3.5.22\x06:\x06ET" {
		t.Errorf("str = %q", m.String())
	}
	m.Reset()
	m.version("0.1.0")
	if m.String() != "U:\x11Gem::Version[\x06I\"\x0a0.1.0\x06:\x06ET" {
		t.Errorf("version = %q", m.String())
	}
	m.Reset()
	m.time(time.Date(2026, 10, 17, 0, 0, 0, 0, time.UTC))
	if want := "Iu:\x09Time\x0d\x20\xa6\x1f\xc0\x00\x00\x00\x00\x06:\x09zoneI\"\x08UTC\x06:\x06EF"; m.String() != want {
		t.Errorf("time = %q, want %q", m.String(), want)
	}
}

func TestQuickSpec(t *testing.T) {
	s, err := ParseGem(testGem(t, "ruby"))
	if err != nil {
		t.Fatal(err)
	}
	q, err := QuickSpec(s)
	if err != nil {
		t.Fatal(err)
	}
	q2, _ := QuickSpec(s)
	if !bytes.Equal(q, q2) {
		t.Fatal("QuickSpec is not deterministic")
	}
	zr, err := zlib.NewReader(bytes.NewReader(q))
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(zr)
	head := "\x04\x08u:\x17Gem::Specification"
	if !strings.HasPrefix(string(raw), head) {
		t.Fatalf("quick spec prefix = %q", raw[:30])
	}
	inner := string(raw[len(head):])
	at := strings.Index(inner, "\x04\x08[\x18")
	var length rbMarshal
	length.fixnum(len(inner) - at)
	if at < 1 || inner[:at] != length.String() {
		t.Fatalf("inner dump header = %q", inner[:8])
	}
	for _, want := range []string{"o:\x14Gem::Dependency", ":\x0cruntime", ":\x10development", "source_code_uri"} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("quick spec missing %q", want)
		}
	}
}
