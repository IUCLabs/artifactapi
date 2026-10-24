package virtual

import (
	"bytes"
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"maps"
	"path"
	"slices"
	"strings"
	"testing"
)

func debStanza(pkg, ver, arch, filename string) string {
	return fmt.Sprintf("Package: %s\nVersion: %s\nArchitecture: %s\nDescription: test\n multi-line body\nFilename: %s\nSize: 1\n", pkg, ver, arch, filename)
}

func flatDebMember(name, date string, stanzas ...string) DebMember {
	rel := "Date: " + date + "\nArchitectures: amd64\nAcquire-By-Hash: no\nSHA256:\n x 1 Packages\n x 1 Packages.gz\n"
	return DebMember{RemoteName: name, Release: []byte(rel), Packages: map[string][]byte{"": []byte(strings.Join(stanzas, "\n"))}}
}

func TestMergeDebDedupesByMemberOrder(t *testing.T) {
	repo := MergeDeb([]DebMember{
		flatDebMember("a", "Mon, 05 Oct 2026 10:00:00 +0000",
			debStanza("foo", "1.0", "amd64", "pool/foo_1.0_amd64.deb"),
			debStanza("bar", "2.0", "amd64", "/pool/bar.deb"),
			debStanza("dot", "1.0", "amd64", "./dot.deb"),
			"Package: noversion\nFilename: nv1.deb\n",
			"Package: noversion\nFilename: nv2.deb\n"),
		flatDebMember("b", "Tue, 06 Oct 2026 10:00:00 UTC",
			debStanza("foo", "1.0", "amd64", "pool/foo-from-b.deb"),
			debStanza("foo", "1.0", "arm64", "pool/foo_arm64.deb"),
			debStanza("foo", "1.1", "amd64", "o/r/releases/download/v1/foo.deb")),
	})
	pk := string(repo.Files["Packages"])
	stanzas := splitStanzas(repo.Files["Packages"])
	if len(stanzas) != 7 {
		t.Fatalf("want 7 stanzas (one duplicate dropped), got %d:\n%s", len(stanzas), pk)
	}
	for _, want := range []string{
		"Filename: a/pool/foo_1.0_amd64.deb",
		"Filename: a/pool/bar.deb",
		"Filename: a/dot.deb",
		"Filename: a/nv1.deb",
		"Filename: a/nv2.deb",
		"Filename: b/pool/foo_arm64.deb",
		"Filename: b/o/r/releases/download/v1/foo.deb",
		" multi-line body",
	} {
		if !strings.Contains(pk, want) {
			t.Errorf("merged Packages missing %q:\n%s", want, pk)
		}
	}
	if strings.Contains(pk, "foo-from-b") {
		t.Fatalf("second member's duplicate must lose:\n%s", pk)
	}
	if !bytes.Equal(gunzip(t, repo.Files["Packages.gz"]), repo.Files["Packages"]) {
		t.Fatal("Packages.gz does not match Packages")
	}
	if !strings.Contains(string(repo.Release), "Date: Tue, 06 Oct 2026 10:00:00 +0000\n") {
		t.Fatalf("Date should be the newest member date:\n%s", repo.Release)
	}
}

func TestMergeDebReleaseHashesFiles(t *testing.T) {
	repo := MergeDeb([]DebMember{flatDebMember("a", "", debStanza("foo", "1", "amd64", "pool/foo.deb"))})
	rel := parseDebRelease(repo.Release)
	if got := slices.Sorted(slices.Values(rel.files)); !slices.Equal(got, []string{"Packages", "Packages.gz"}) {
		t.Fatalf("Release lists %v", got)
	}
	inSHA := false
	checked := 0
	for line := range strings.SplitSeq(string(repo.Release), "\n") {
		if !strings.HasPrefix(line, " ") {
			inSHA = line == "SHA256:"
			continue
		}
		var sum, name string
		var size int
		if _, err := fmt.Sscan(line, &sum, &size, &name); err != nil {
			t.Fatal(err)
		}
		body := repo.Files[name]
		want := sha256Hex(body)
		if !inSHA {
			h := md5.Sum(body)
			want = hex.EncodeToString(h[:])
		}
		if sum != want || size != len(body) {
			t.Errorf("%s: Release has %s %d, file is %s %d", name, sum, size, want, len(body))
		}
		checked++
	}
	if checked != 4 {
		t.Fatalf("checked %d Release entries, want 4:\n%s", checked, repo.Release)
	}
	if !bytes.Equal(MergeDeb([]DebMember{flatDebMember("a", "", debStanza("foo", "1", "amd64", "pool/foo.deb"))}).Release, repo.Release) {
		t.Fatal("merge must be deterministic")
	}
}

func TestMergeDebSuiteLayout(t *testing.T) {
	rel := func(comps string) []byte {
		return []byte("Suite: stable\nCodename: trixie\nArchitectures: amd64 arm64\nComponents: " + comps + "\nSHA256:\n x 1 main/binary-amd64/Packages.xz\n")
	}
	repo := MergeDeb([]DebMember{
		{RemoteName: "deb1", Release: rel("main"), Packages: map[string][]byte{"main/binary-amd64/": []byte(debStanza("foo", "1", "amd64", "pool/main/f/foo.deb"))}},
		{RemoteName: "deb2", Release: rel("main contrib"), Packages: map[string][]byte{
			"main/binary-amd64/":    []byte(debStanza("bar", "1", "amd64", "pool/main/b/bar.deb")),
			"contrib/binary-amd64/": []byte(debStanza("baz", "1", "amd64", "pool/contrib/b/baz.deb")),
		}},
	})
	for _, want := range []string{"Suite: stable\n", "Codename: trixie\n", "Components: main contrib\n", "Architectures: amd64 arm64\n"} {
		if !strings.Contains(string(repo.Release), want) {
			t.Errorf("Release missing %q:\n%s", want, repo.Release)
		}
	}
	want := []string{"contrib/binary-amd64/Packages", "contrib/binary-amd64/Packages.gz", "main/binary-amd64/Packages", "main/binary-amd64/Packages.gz"}
	if got := slices.Sorted(slices.Values(parseDebRelease(repo.Release).files)); !slices.Equal(got, want) {
		t.Fatalf("Release lists %v, want %v", got, want)
	}
	if !strings.Contains(string(repo.Release), "Acquire-By-Hash: yes\n") {
		t.Fatalf("Release must advertise by-hash:\n%s", repo.Release)
	}
	for _, n := range want {
		byHash := path.Dir(n) + "/by-hash/SHA256/" + sha256Hex(repo.Files[n])
		if !bytes.Equal(repo.Files[byHash], repo.Files[n]) {
			t.Errorf("%s not served at %s", n, byHash)
		}
	}
	if len(repo.Files) != 2*len(want) {
		t.Fatalf("files %v, want each index plus its by-hash copy", slices.Sorted(maps.Keys(repo.Files)))
	}
	if n := len(splitStanzas(repo.Files["main/binary-amd64/Packages"])); n != 2 {
		t.Fatalf("main/binary-amd64 should merge both members, got %d stanzas", n)
	}
}

func TestDebIndexFiles(t *testing.T) {
	rel := parseDebRelease([]byte("Origin: Debian\nMD5Sum:\n a 1 main/binary-amd64/Packages\n a 1 main/binary-amd64/Packages.gz\nSHA256:\n" +
		" a 1 main/binary-amd64/Packages\n a 1 main/binary-amd64/Packages.xz\n a 1 main/binary-amd64/Packages.gz\n" +
		" a 1 main/debian-installer/binary-amd64/Packages.xz\n a 1 main/i18n/Translation-en.xz\n a 1 contrib/binary-all/Packages\n"))
	got := debIndexFiles(rel)
	want := map[string]string{"main/binary-amd64/": "main/binary-amd64/Packages.xz", "contrib/binary-all/": "contrib/binary-all/Packages"}
	if !maps.Equal(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	if flat := debIndexFiles(parseDebRelease([]byte("SHA256:\n a 1 Packages\n a 1 Packages.gz\n"))); flat[""] != "Packages.gz" {
		t.Fatalf("flat repo should fetch Packages.gz, got %v", flat)
	}
}

func TestDebIndexPath(t *testing.T) {
	for _, tc := range []struct {
		in, base, name string
		ok             bool
	}{
		{"Release", "", "Release", true},
		{"Packages.gz", "", "Packages.gz", true},
		{"dists/trixie/Release", "dists/trixie/", "Release", true},
		{"dists/trixie/main/binary-amd64/Packages", "dists/trixie/", "main/binary-amd64/Packages", true},
		{"by-hash/SHA256/abababababababababababababababababababababababababababababababab", "", "by-hash/SHA256/abababababababababababababababababababababababababababababababab", true},
		{"dists/trixie/main/binary-amd64/by-hash/SHA256/abababababababababababababababababababababababababababababababab", "dists/trixie/", "main/binary-amd64/by-hash/SHA256/abababababababababababababababababababababababababababababababab", true},
		{"dists/trixie/main/binary-amd64/by-hash/MD5Sum/abababababababababababababababab", "", "", false},
		{"InRelease", "", "", false},
		{"Release.gpg", "", "", false},
		{"dists/trixie/InRelease", "", "", false},
		{"dists/trixie/main/i18n/Translation-en", "", "", false},
		{"dists/Release", "", "", false},
		{"pool/foo.deb", "", "", false},
	} {
		base, name, ok := debIndexPath(tc.in)
		if base != tc.base || name != tc.name || ok != tc.ok {
			t.Errorf("%s: got (%q, %q, %v), want (%q, %q, %v)", tc.in, base, name, ok, tc.base, tc.name, tc.ok)
		}
	}
}
