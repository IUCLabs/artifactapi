package v1

import "testing"

func TestParseDockerPath(t *testing.T) {
	tests := []struct {
		name      string
		rest      string
		wantOK    bool
		wantImage string
		wantKind  string
		wantRef   string
	}{
		{"start upload trailing slash", "team/app/blobs/uploads/", true, "team/app", "upload", ""},
		{"start upload no slash", "team/app/blobs/uploads", true, "team/app", "upload", ""},
		{"patch upload with uuid", "team/app/blobs/uploads/abc-123", true, "team/app", "upload", "abc-123"},
		{"single-segment image upload", "app/blobs/uploads/", true, "app", "upload", ""},
		{"blob by digest", "team/app/blobs/sha256:deadbeef", true, "team/app", "blob", "sha256:deadbeef"},
		{"manifest by tag", "team/app/manifests/v1.0.0", true, "team/app", "manifest", "v1.0.0"},
		{"manifest by digest", "team/app/manifests/sha256:cafe", true, "team/app", "manifest", "sha256:cafe"},
		{"tags list", "team/app/tags/list", true, "team/app", "tags", ""},
		{"leading slash tolerated", "/team/app/manifests/latest", true, "team/app", "manifest", "latest"},
		{"deep image name", "a/b/c/manifests/latest", true, "a/b/c", "manifest", "latest"},
		{"unrecognised", "team/app/whatever", false, "", "", ""},
		{"tags list without image", "tags/list", false, "", "", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := parseDockerPath(tc.rest)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if !tc.wantOK {
				return
			}
			if got.image != tc.wantImage || got.kind != tc.wantKind || got.ref != tc.wantRef {
				t.Fatalf("got %+v, want image=%q kind=%q ref=%q", got, tc.wantImage, tc.wantKind, tc.wantRef)
			}
		})
	}
}

func TestIsDigest(t *testing.T) {
	if !isDigest("sha256:abc") {
		t.Fatal("sha256: prefix should be a digest")
	}
	if isDigest("v1.0.0") {
		t.Fatal("a tag is not a digest")
	}
}
