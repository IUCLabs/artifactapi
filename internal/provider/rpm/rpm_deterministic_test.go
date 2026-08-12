package rpm

import (
	"bytes"
	"compress/gzip"
	"encoding/xml"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/IUCLabs/artifactapi/internal/provider"
)

func gunzip(t *testing.T, data []byte) string {
	t.Helper()
	zr, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("gzip reader: %v", err)
	}
	out, err := io.ReadAll(zr)
	if err != nil {
		t.Fatalf("gunzip: %v", err)
	}
	return string(out)
}

// sampleMetas returns a fixed two-package repo state whose upload timestamps are
// pinned, so any nondeterminism must come from the generators themselves.
func sampleMetas() []provider.RPMMetadata {
	base := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	return []provider.RPMMetadata{
		{
			Name: "alpha", Version: "1.0", Release: "1", Arch: "x86_64",
			Summary: "a", Description: "d", ContentHash: "sha256:aaa",
			FilePath: "Packages/alpha-1.0-1.x86_64.rpm", RPMSize: 10, InstalledSize: 20,
			Provides:  []provider.RPMDep{{Name: "alpha"}},
			Requires:  []provider.RPMDep{{Name: "libc", Flags: "GE", Version: "2.0"}},
			CreatedAt: base,
		},
		{
			Name: "beta", Version: "2.0", Release: "3", Arch: "noarch",
			Summary: "b", Description: "d2", ContentHash: "sha256:bbb",
			FilePath: "Packages/beta-2.0-3.noarch.rpm", RPMSize: 30, InstalledSize: 40,
			CreatedAt: base.Add(time.Hour),
		},
	}
}

// TestRepodataGeneratorsDeterministic is the direct regression guard for #117:
// generating each metadata document twice from identical state must yield
// byte-identical output (hence an identical sha256). The old code embedded
// time.Now() inside primary.xml.gz, so its bytes/hash drifted every second.
func TestRepodataGeneratorsDeterministic(t *testing.T) {
	metas := sampleMetas()
	gens := map[string]func([]provider.RPMMetadata) []byte{
		"primary":   generatePrimaryXMLGZ,
		"filelists": generateFilelistsXMLGZ,
		"other":     generateOtherXMLGZ,
	}
	for name, gen := range gens {
		a := gen(metas)
		b := gen(metas)
		if sha256Hex(a) != sha256Hex(b) {
			t.Errorf("%s: sha256 differs between two generations (nondeterministic): %s != %s",
				name, sha256Hex(a), sha256Hex(b))
		}
	}

	// repomd.xml itself must also be byte-stable across regenerations.
	r1 := generateRepomd(repomdRevision(metas), sha256Hex(generatePrimaryXMLGZ(metas)), 1, "f", 2, "o", 3)
	r2 := generateRepomd(repomdRevision(metas), sha256Hex(generatePrimaryXMLGZ(metas)), 1, "f", 2, "o", 3)
	if string(r1) != string(r2) {
		t.Error("repomd.xml differs between two generations")
	}
}

// TestPrimaryTimeUsesPersistedCreatedAt proves the <time> element is a pure
// function of the persisted upload timestamp, not the wall clock.
func TestPrimaryTimeUsesPersistedCreatedAt(t *testing.T) {
	metas := sampleMetas()
	out := gunzip(t, generatePrimaryXMLGZ(metas))
	if want := `<time file="1767323045" build="0"/>`; !strings.Contains(out, want) {
		t.Errorf("primary.xml missing persisted <time> %q; got:\n%s", want, out)
	}
	// A zero (unset) CreatedAt collapses to a fixed 0, never a live clock value.
	metas[0].CreatedAt = time.Time{}
	out = gunzip(t, generatePrimaryXMLGZ(metas))
	if !strings.Contains(out, `<time file="0" build="0"/>`) {
		t.Errorf("zero CreatedAt should emit file=\"0\"; got:\n%s", out)
	}
}

type repomdDoc struct {
	Revision string `xml:"revision"`
	Data     []struct {
		Type     string `xml:"type,attr"`
		Checksum struct {
			Value string `xml:",chardata"`
		} `xml:"checksum"`
		Location struct {
			Href string `xml:"href,attr"`
		} `xml:"location"`
	} `xml:"data"`
}

// TestRepomdHashMatchesServedBytes asserts the exact invariant #117 violated:
// the sha256 advertised in repomd.xml equals the sha256 of the bytes the
// content-addressed serve* handler returns for the same repo state.
func TestRepomdHashMatchesServedBytes(t *testing.T) {
	p := &Provider{}
	reader := fakeRPMReader{metas: sampleMetas()}

	serve := func(path string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodGet, "/"+path, nil)
		if !p.ServeLocalIndex(w, r, reader, "repo", path) {
			t.Fatalf("ServeLocalIndex false for %q", path)
		}
		if w.Code != http.StatusOK {
			t.Fatalf("%s: code %d", path, w.Code)
		}
		return w
	}

	var doc repomdDoc
	if err := xml.Unmarshal(serve("repodata/repomd.xml").Body.Bytes(), &doc); err != nil {
		t.Fatalf("parse repomd: %v", err)
	}
	if len(doc.Data) != 3 {
		t.Fatalf("expected 3 <data> entries, got %d", len(doc.Data))
	}

	for _, d := range doc.Data {
		// The advertised location is content-addressed: repodata/<sha256>-<type>.xml.gz.
		body := serve("repodata/" + d.Location.Href[len("repodata/"):]).Body.Bytes()
		got := sha256Hex(body)
		if got != d.Checksum.Value {
			t.Errorf("%s: repomd advertises %s but served bytes hash to %s (dnf would reject)",
				d.Type, d.Checksum.Value, got)
		}
		if d.Location.Href != "repodata/"+d.Checksum.Value+"-"+d.Type+".xml.gz" {
			t.Errorf("%s: location %q not addressed by its checksum %s", d.Type, d.Location.Href, d.Checksum.Value)
		}
	}
}
