//go:build e2e

package e2e

import (
	"net/http"
	"testing"
)

func TestProxyUnknownRemote(t *testing.T) {
	assertStatus(t, apiURL("/api/v1/remote/nonexistent/some/path"), http.StatusNotFound)
}

func TestProxyBlocklist(t *testing.T) {
	createRemote(t, `{
		"name": "blocklist-test",
		"package_type": "generic",
		"base_url": "https://example.com",
		"blocklist": ["\\.exe$"],
		"stale_on_error": true
	}`)
	defer deleteRemote(t, "blocklist-test")

	assertStatus(t, apiURL("/api/v1/remote/blocklist-test/malware.exe"), http.StatusForbidden)
}

func TestProxyPatterns(t *testing.T) {
	createRemote(t, `{
		"name": "patterns-test",
		"package_type": "generic",
		"base_url": "https://example.com",
		"patterns": ["^releases/"],
		"stale_on_error": true
	}`)
	defer deleteRemote(t, "patterns-test")

	assertStatus(t, apiURL("/api/v1/remote/patterns-test/uploads/file.tar.gz"), http.StatusForbidden)
}
