//go:build dockere2e

package e2edocker

import (
	"bytes"
	"fmt"
	"net/http"
	"testing"
)

type cachingCase struct {
	pkgType string
	// path is the request path under /api/v1/remote/<name>/. The provider
	// derives the upstream URL from it (docker prepends /v2/, terraform
	// prepends /v1/providers/), and the fixture lives at that resolved path.
	path    string
	fixture string
}

// cachingCases is filled by one caching_<type>_test.go file per package type.
var cachingCases []cachingCase

// TestCachingPerProvider proxies one immutable artifact for every remote
// package type through the mock upstream and asserts: first fetch is served
// from the remote, the second from cache, and the bytes match the origin.
func TestCachingPerProvider(t *testing.T) {
	for _, tc := range cachingCases {
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
