package deb

import (
	"bytes"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/IUCLabs/artifactapi/internal/provider"
)

// debFixture returns a fixed set of rows with persisted created_at values, in
// the total order ListDebMetadataEntries produces (name, version, arch,
// file_path), so the generators are exercised on a stable input.
func debFixture() []provider.DebMetadata {
	t1 := time.Date(2026, 3, 1, 8, 30, 0, 0, time.UTC)
	t2 := time.Date(2026, 4, 15, 12, 0, 0, 0, time.UTC) // newest
	return []provider.DebMetadata{
		{
			RepoName: "r", FilePath: "pool/aaa_1.0_amd64.deb", ContentHash: "sha256:aa",
			Name: "aaa", Version: "1.0", Architecture: "amd64",
			Control: "Package: aaa\nVersion: 1.0\nArchitecture: amd64",
			Size:    100, MD5: "d41d8cd98f00b204e9800998ecf8427e", SHA256: "aa", CreatedAt: t1,
		},
		{
			RepoName: "r", FilePath: "pool/bbb_2.0_arm64.deb", ContentHash: "sha256:bb",
			Name: "bbb", Version: "2.0", Architecture: "arm64",
			Control: "Package: bbb\nVersion: 2.0\nArchitecture: arm64",
			Size:    200, MD5: "0cc175b9c0f1b6a831c399e269772661", SHA256: "bb", CreatedAt: t2,
		},
	}
}

// TestDebGeneratorsDeterministic asserts the served bytes are a pure function of
// DB state: Packages, Packages.gz and Release are byte-identical across two
// generations separated by wall-clock time. Fails against the old
// time.Now()-stamped Release Date:.
func TestDebGeneratorsDeterministic(t *testing.T) {
	metas := debFixture()

	pkgs1 := generatePackages(metas)
	rel1 := generateRelease(metas)
	gz1 := gzipBytes(pkgs1)

	time.Sleep(10 * time.Millisecond)

	pkgs2 := generatePackages(metas)
	rel2 := generateRelease(metas)
	gz2 := gzipBytes(pkgs2)

	if !bytes.Equal(pkgs1, pkgs2) {
		t.Error("Packages differs across generations")
	}
	if !bytes.Equal(gz1, gz2) {
		t.Error("Packages.gz differs across generations")
	}
	if !bytes.Equal(rel1, rel2) {
		t.Errorf("Release differs across generations:\n--- first ---\n%s\n--- second ---\n%s", rel1, rel2)
	}
}

// TestDebReleaseDateUsesPersistedCreatedAt pins the Release Date: to the newest
// persisted created_at (RFC1123Z, UTC), not wall clock. Fails against the old
// time.Now() code.
func TestDebReleaseDateUsesPersistedCreatedAt(t *testing.T) {
	metas := debFixture()
	want := time.Date(2026, 4, 15, 12, 0, 0, 0, time.UTC).Format(time.RFC1123Z)

	rel := string(generateRelease(metas))
	var got string
	for _, line := range strings.Split(rel, "\n") {
		if strings.HasPrefix(line, "Date:") {
			got = strings.TrimSpace(strings.TrimPrefix(line, "Date:"))
			break
		}
	}
	if got != want {
		t.Errorf("Release Date: = %q, want %q (newest created_at)", got, want)
	}
}

// TestDebReleaseDateEmptyRepoIsEpoch guards the fallback: an empty repo yields a
// deterministic epoch Date: rather than wall clock.
func TestDebReleaseDateEmptyRepoIsEpoch(t *testing.T) {
	want := time.Unix(0, 0).UTC().Format(time.RFC1123Z)
	rel := string(generateRelease(nil))
	if !strings.Contains(rel, "Date: "+want+"\n") {
		t.Errorf("empty-repo Release missing epoch Date: %q\n%s", want, rel)
	}
}

// TestDebReleaseChecksumsMatchServedBytes is the exact apt invariant: the
// sha256/size (and md5/size) advertised for Packages and Packages.gz in Release
// equal the sha256/size of the actual bytes ServeLocalIndex serves. apt rejects
// any mismatch.
func TestDebReleaseChecksumsMatchServedBytes(t *testing.T) {
	metas := debFixture()

	packages := generatePackages(metas)
	packagesGz := gzipBytes(packages)
	rel := string(generateRelease(metas))

	wantSHA := map[string]struct {
		hash string
		size int
	}{
		"Packages":    {sha256Hex(packages), len(packages)},
		"Packages.gz": {sha256Hex(packagesGz), len(packagesGz)},
	}
	wantMD5 := map[string]struct {
		hash string
		size int
	}{
		"Packages":    {md5Hex(packages), len(packages)},
		"Packages.gz": {md5Hex(packagesGz), len(packagesGz)},
	}

	sha := parseReleaseSection(rel, "SHA256:")
	md5s := parseReleaseSection(rel, "MD5Sum:")

	for name, w := range wantSHA {
		got, ok := sha[name]
		if !ok {
			t.Fatalf("Release SHA256 section missing %q", name)
		}
		if got.hash != w.hash || got.size != w.size {
			t.Errorf("Release SHA256 %s = (%s, %d), served bytes are (%s, %d)", name, got.hash, got.size, w.hash, w.size)
		}
	}
	for name, w := range wantMD5 {
		got, ok := md5s[name]
		if !ok {
			t.Fatalf("Release MD5Sum section missing %q", name)
		}
		if got.hash != w.hash || got.size != w.size {
			t.Errorf("Release MD5Sum %s = (%s, %d), served bytes are (%s, %d)", name, got.hash, got.size, w.hash, w.size)
		}
	}
}

type releaseEntry struct {
	hash string
	size int
}

// parseReleaseSection reads the indented " <hash> <size> <name>" lines that
// follow a "SHA256:" / "MD5Sum:" header until the next non-indented line.
func parseReleaseSection(release, header string) map[string]releaseEntry {
	out := map[string]releaseEntry{}
	lines := strings.Split(release, "\n")
	in := false
	for _, line := range lines {
		if line == header {
			in = true
			continue
		}
		if !in {
			continue
		}
		if !strings.HasPrefix(line, " ") {
			break
		}
		fields := strings.Fields(line)
		if len(fields) != 3 {
			continue
		}
		size, _ := strconv.Atoi(fields[1])
		out[fields[2]] = releaseEntry{hash: fields[0], size: size}
	}
	return out
}
