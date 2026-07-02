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

func TestProxyHeadBlocklist(t *testing.T) {
	createRemote(t, `{
		"name": "head-block-test",
		"package_type": "generic",
		"base_url": "https://example.com",
		"blocklist": ["\\.exe$"],
		"stale_on_error": true
	}`)
	defer deleteRemote(t, "head-block-test")

	req, _ := http.NewRequest(http.MethodHead, apiURL("/v2/head-block-test/malware.exe"), nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("HEAD: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("HEAD blocklisted path: got %d, want 403", resp.StatusCode)
	}
}

func TestProxyHeadUnknownRemote(t *testing.T) {
	req, _ := http.NewRequest(http.MethodHead, apiURL("/v2/nonexistent/some/path"), nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("HEAD: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("HEAD unknown remote: got %d, want 404", resp.StatusCode)
	}
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
