package v1

import (
	"crypto/sha1"
	"crypto/sha512"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/IUCLabs/artifactapi/internal/database"
)

func TestNPMPackageName(t *testing.T) {
	tests := map[string]string{
		"left-pad":                "left-pad",
		"left-pad/":               "left-pad",
		"@acme%2fwidget":          "@acme/widget",
		"@acme%2Fwidget":          "@acme/widget",
		"@acme/widget":            "@acme/widget",
		"left-pad/-/left-pad.tgz": "",
		"@acme/widget/-/w-1.tgz":  "",
		"a/b":                     "",
		"../etc":                  "",
		"Upper":                   "",
		"":                        "",
	}
	for in, want := range tests {
		if got := npmPackageName(in); got != want {
			t.Errorf("npmPackageName(%q) = %q, want %q", in, got, want)
		}
	}
}

type publishOpts struct {
	bodyName, manifestName, version, integrity, shasum string
	tags                                               map[string]string
	extraVersion                                       bool
}

func publishBody(t *testing.T, pkg string, tgz []byte, o publishOpts) string {
	t.Helper()
	sha := sha512.Sum512(tgz)
	sh1 := sha1.Sum(tgz)
	if o.bodyName == "" {
		o.bodyName = pkg
	}
	if o.manifestName == "" {
		o.manifestName = pkg
	}
	if o.version == "" {
		o.version = "1.0.0"
	}
	if o.integrity == "" {
		o.integrity = "sha512-" + base64.StdEncoding.EncodeToString(sha[:])
	}
	if o.shasum == "" {
		o.shasum = hex.EncodeToString(sh1[:])
	}
	if o.tags == nil {
		o.tags = map[string]string{"latest": o.version}
	}
	versions := map[string]any{o.version: map[string]any{
		"name": o.manifestName, "version": o.version, "main": "index.js",
		"dist": map[string]any{"integrity": o.integrity, "shasum": o.shasum, "tarball": "http://elsewhere/x.tgz"},
	}}
	if o.extraVersion {
		versions["2.0.0"] = map[string]any{"name": pkg, "version": "2.0.0"}
	}
	b, _ := json.Marshal(map[string]any{
		"_id": pkg, "name": o.bodyName, "dist-tags": o.tags, "versions": versions,
		"_attachments": map[string]any{pkg + "-" + o.version + ".tgz": map[string]any{
			"content_type": "application/octet-stream", "data": base64.StdEncoding.EncodeToString(tgz), "length": len(tgz),
		}},
	})
	return string(b)
}

func TestParseNPMPublish(t *testing.T) {
	tgz := []byte("fake tarball bytes")

	rel, err := parseNPMPublish(strings.NewReader(publishBody(t, "@acme/widget", tgz, publishOpts{tags: map[string]string{"latest": "1.0.0", "beta": "1.0.0"}})), "@acme/widget")
	if err != nil {
		t.Fatalf("valid scoped publish rejected: %v", err)
	}
	if rel.version != "1.0.0" || string(rel.tarball) != string(tgz) || len(rel.tags) != 2 {
		t.Fatalf("unexpected release: %+v", rel)
	}

	if _, err := parseNPMPublish(strings.NewReader(publishBody(t, "left-pad", tgz, publishOpts{integrity: "none", shasum: hex.EncodeToString(make([]byte, 20))})), "left-pad"); err == nil {
		t.Fatal("shasum fallback should reject a mismatched tarball")
	}
	if _, err := parseNPMPublish(strings.NewReader(publishBody(t, "left-pad", tgz, publishOpts{integrity: "none"})), "left-pad"); err != nil {
		t.Fatalf("matching shasum fallback rejected: %v", err)
	}

	bad := map[string]publishOpts{
		"body name mismatch":        {bodyName: "other"},
		"manifest name mismatch":    {manifestName: "other"},
		"integrity mismatch":        {integrity: "sha512-AAAA"},
		"path-like version":         {version: "../1.0.0"},
		"non-semver version":        {version: "1..2"},
		"leading-zero version":      {version: "01.0.0"},
		"tag at another version":    {tags: map[string]string{"latest": "9.9.9"}},
		"path-like tag":             {tags: map[string]string{"../x": "1.0.0"}},
		"multiple versions at once": {extraVersion: true},
	}
	for name, o := range bad {
		if _, err := parseNPMPublish(strings.NewReader(publishBody(t, "left-pad", tgz, o)), "left-pad"); err == nil {
			t.Errorf("%s: expected rejection", name)
		}
	}
	if _, err := parseNPMPublish(strings.NewReader(`{"name":"left-pad","versions":{}}`), "left-pad"); err == nil {
		t.Error("deprecate-style body without attachments should be rejected")
	}
}

func TestBuildNPMPackument(t *testing.T) {
	blobs := map[string]string{
		"h1": `{"name":"my_pkg","version":"1.0.0","dist":{"shasum":"aa","tarball":"http://elsewhere/x.tgz"}}`,
		"h2": `{"name":"my_pkg","version":"1.1.0"}`,
		"h3": "1.1.0",
		"h4": "2.0.0",
		"hx": `{"name":"myXpkg","version":"6.6.6"}`,
	}
	read := func(h string) ([]byte, error) {
		b, ok := blobs[h]
		if !ok {
			return nil, errors.New("missing blob")
		}
		return []byte(b), nil
	}
	files := []database.LocalFile{
		{FilePath: "my_pkg/-/dist-tags/latest", ContentHash: "h3"},
		{FilePath: "my_pkg/-/dist-tags/next", ContentHash: "h4"},
		{FilePath: "my_pkg/-/my_pkg-1.0.0.json", ContentHash: "h1"},
		{FilePath: "my_pkg/-/my_pkg-1.0.0.tgz", ContentHash: "nope"},
		{FilePath: "my_pkg/-/my_pkg-1.1.0.json", ContentHash: "h2"},
		{FilePath: "myXpkg/-/myXpkg-6.6.6.json", ContentHash: "hx"},
	}

	doc, err := buildNPMPackument("my_pkg", files, read, "http://reg/api/v1/local/npm/")
	if err != nil {
		t.Fatal(err)
	}
	if tags := doc["dist-tags"].(map[string]string); tags["latest"] != "1.1.0" || len(tags) != 1 {
		t.Errorf("dist-tags = %v", tags)
	}
	versions := doc["versions"].(map[string]any)
	if len(versions) != 2 {
		t.Fatalf("versions = %v (LIKE over-match must be dropped)", versions)
	}
	v1 := versions["1.0.0"].(map[string]any)["dist"].(map[string]any)
	if v1["tarball"] != "http://reg/api/v1/local/npm/my_pkg/-/my_pkg-1.0.0.tgz" || v1["shasum"] != "aa" {
		t.Errorf("1.0.0 dist = %v", v1)
	}
	if versions["1.1.0"].(map[string]any)["dist"].(map[string]any)["tarball"] != "http://reg/api/v1/local/npm/my_pkg/-/my_pkg-1.1.0.tgz" {
		t.Error("1.1.0 missing rewritten tarball")
	}

	if doc, err := buildNPMPackument("absent", files, read, "x/"); err != nil || doc != nil {
		t.Errorf("absent package: doc=%v err=%v", doc, err)
	}
}
