//go:build dockere2e

package e2edocker

import (
	"bytes"
	"fmt"
	"net/http"
	"testing"
)

// TestCachingPerProvider proxies one immutable artifact for every remote
// package type through the mock upstream and asserts: first fetch is served
// from the remote, the second from cache, and the bytes match the origin.
func TestCachingPerProvider(t *testing.T) {
	cases := []struct {
		pkgType string
		// path is the request path under /api/v1/remote/<name>/. The provider
		// derives the upstream URL from it (docker prepends /v2/, terraform
		// prepends /v1/providers/), and the fixture lives at that resolved path.
		path    string
		fixture string
	}{
		{"generic", "blobs/hello.bin", "blobs/hello.bin"},
		{"npm", "mypkg/-/mypkg-1.0.0.tgz", "mypkg/-/mypkg-1.0.0.tgz"},
		{"helm", "charts/mychart-1.0.0.tgz", "charts/mychart-1.0.0.tgz"},
		{"pypi", "packages/foo-1.0-py3-none-any.whl", "packages/foo-1.0-py3-none-any.whl"},
		{"rpm", "rpmrepo/Packages/e2e-testpkg-1.0-1.noarch.rpm", "rpmrepo/Packages/e2e-testpkg-1.0-1.noarch.rpm"},
		{"alpine", "alpine/x86_64/testpkg-1.0-r0.apk", "alpine/x86_64/testpkg-1.0-r0.apk"},
		{"puppet", "puppet-releases/author-mod-1.0.0.tar.gz", "puppet-releases/author-mod-1.0.0.tar.gz"},
		{"goproxy", "goproxy/example.com/mod/@v/v1.0.0.zip", "goproxy/example.com/mod/@v/v1.0.0.zip"},
		{"cargo", "crates/mycrate/mycrate-1.0.0.crate", "crates/mycrate/mycrate-1.0.0.crate"},
		{"terraform", "hashicorp/aws/download/pkg.zip", "v1/providers/hashicorp/aws/download/pkg.zip"},
		{"docker", "library/testimg/blobs/blobdata", "v2/library/testimg/blobs/blobdata"},
	}

	for _, tc := range cases {
		t.Run(tc.pkgType, func(t *testing.T) {
			name := "cache-" + tc.pkgType
			createRepo(t, fmt.Sprintf(`{
				"name": %q,
				"package_type": %q,
				"repo_type": "remote",
				"base_url": %q,
				"stale_on_error": true
			}`, name, tc.pkgType, mockUpstream()))
			defer deleteRepo(t, name)

			want := fixtureBytes(t, tc.fixture)
			url := api("/api/v1/remote/" + name + "/" + tc.path)

			// First fetch: from remote.
			resp, body := doRequest(t, http.MethodGet, url, nil, "")
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("first fetch: status %d: %s", resp.StatusCode, body)
			}
			if src := resp.Header.Get("X-Artifact-Source"); src != "remote" {
				t.Fatalf("first fetch source = %q, want remote", src)
			}
			if !bytes.Equal(body, want) {
				t.Fatalf("first fetch body mismatch: got %d bytes, want %d", len(body), len(want))
			}

			// Second fetch: from cache, identical bytes.
			resp, body = doRequest(t, http.MethodGet, url, nil, "")
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("second fetch: status %d: %s", resp.StatusCode, body)
			}
			if src := resp.Header.Get("X-Artifact-Source"); src != "cache" {
				t.Fatalf("second fetch source = %q, want cache", src)
			}
			if !bytes.Equal(body, want) {
				t.Fatalf("cached body mismatch: got %d bytes, want %d", len(body), len(want))
			}
		})
	}
}
