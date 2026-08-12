package alpine

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"io"
	"testing"
	"time"

	"github.com/IUCLabs/artifactapi/internal/provider"
)

func apkFixture() []provider.AlpineMetadata {
	return []provider.AlpineMetadata{
		{
			RepoName: "r", FilePath: "x86_64/aaa-1.0-r0.apk", Checksum: "Q1aaa",
			Name: "aaa", Version: "1.0-r0", Arch: "x86_64", DownloadSize: 100, InstalledSize: 10,
			Description: "pkg aaa", URL: "https://a", License: "MIT",
			Depends: []string{"so:libc"}, Provides: []string{"cmd:aaa"}, BuildTime: 1710000000,
		},
		{
			RepoName: "r", FilePath: "x86_64/bbb-2.0-r0.apk", Checksum: "Q1bbb",
			Name: "bbb", Version: "2.0-r0", Arch: "x86_64", DownloadSize: 200, InstalledSize: 20,
		},
	}
}

// TestAPKIndexDeterministic asserts APKINDEX.tar.gz is byte-identical across two
// generations separated by wall-clock time, so the two no-affinity replicas and
// every regeneration serve the same bytes (issue #117).
func TestAPKIndexDeterministic(t *testing.T) {
	metas := apkFixture()

	first := generateAPKIndex(metas)
	time.Sleep(10 * time.Millisecond)
	second := generateAPKIndex(metas)

	if !bytes.Equal(first, second) {
		t.Error("APKINDEX.tar.gz differs across generations")
	}
}

// TestAPKIndexTarModTimePinned guards the tar header: its ModTime must be the
// pinned Unix epoch, never wall clock. Fails if a future edit stamps time.Now().
func TestAPKIndexTarModTimePinned(t *testing.T) {
	metas := apkFixture()

	zr, err := gzip.NewReader(bytes.NewReader(generateAPKIndex(metas)))
	if err != nil {
		t.Fatalf("gzip: %v", err)
	}
	if !zr.ModTime.IsZero() && zr.ModTime.Unix() != 0 {
		t.Errorf("gzip header ModTime = %v, want zero/epoch", zr.ModTime)
	}

	tarBytes, err := io.ReadAll(zr)
	if err != nil {
		t.Fatalf("gunzip: %v", err)
	}
	tr := tar.NewReader(bytes.NewReader(tarBytes))
	hdr, err := tr.Next()
	if err != nil {
		t.Fatalf("tar: %v", err)
	}
	if hdr.Name != "APKINDEX" {
		t.Fatalf("tar entry = %q, want APKINDEX", hdr.Name)
	}
	if hdr.ModTime.Unix() != 0 {
		t.Errorf("APKINDEX tar ModTime = %v (unix %d), want epoch (0)", hdr.ModTime, hdr.ModTime.Unix())
	}
}
