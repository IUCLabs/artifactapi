//go:build dockere2e

package e2edocker

import (
	"encoding/json"
	"net/http"
	"testing"
)

func TestHealth(t *testing.T) {
	resp, body := doRequest(t, http.MethodGet, api("/health"), nil, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("health: status %d: %s", resp.StatusCode, body)
	}
}

// TestRemoteLifecycle covers add/change/delete for a remote repository.
func TestRemoteLifecycle(t *testing.T) {
	createRepo(t, `{
		"name": "crud-remote",
		"package_type": "generic",
		"repo_type": "remote",
		"base_url": "https://example.com",
		"mutable_ttl": 600,
		"stale_on_error": true
	}`)
	defer deleteRepo(t, "crud-remote")

	got := getRepo(t, "crud-remote")
	if got["base_url"] != "https://example.com" || got["mutable_ttl"].(float64) != 600 {
		t.Fatalf("unexpected created remote: %v", got)
	}

	// change
	resp, body := doRequest(t, http.MethodPut, api("/api/v2/remotes/crud-remote"), []byte(`{
		"package_type": "generic",
		"base_url": "https://updated.example.com",
		"mutable_ttl": 120,
		"stale_on_error": true
	}`), "application/json")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("update remote: status %d: %s", resp.StatusCode, body)
	}
	got = getRepo(t, "crud-remote")
	if got["base_url"] != "https://updated.example.com" || got["mutable_ttl"].(float64) != 120 {
		t.Fatalf("update not applied: %v", got)
	}

	// delete
	resp, _ = doRequest(t, http.MethodDelete, api("/api/v2/remotes/crud-remote"), nil, "")
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete remote: status %d", resp.StatusCode)
	}
	resp, _ = doRequest(t, http.MethodGet, api("/api/v2/remotes/crud-remote"), nil, "")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404 after delete, got %d", resp.StatusCode)
	}
}

// TestLocalLifecycle covers add/delete for a local repository.
func TestLocalLifecycle(t *testing.T) {
	createRepo(t, `{
		"name": "crud-local",
		"package_type": "generic",
		"repo_type": "local"
	}`)
	defer deleteRepo(t, "crud-local")

	got := getRepo(t, "crud-local")
	if got["repo_type"] != "local" {
		t.Fatalf("expected repo_type local, got %v", got["repo_type"])
	}

	resp, _ := doRequest(t, http.MethodDelete, api("/api/v2/remotes/crud-local"), nil, "")
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete local: status %d", resp.StatusCode)
	}
}

// TestVirtualLifecycle covers add/change/delete for a virtual repository.
func TestVirtualLifecycle(t *testing.T) {
	createRepo(t, `{"name":"vmem-a","package_type":"helm","repo_type":"remote","base_url":"https://a.example.com","stale_on_error":true}`)
	createRepo(t, `{"name":"vmem-b","package_type":"helm","repo_type":"remote","base_url":"https://b.example.com","stale_on_error":true}`)
	defer deleteRepo(t, "vmem-a")
	defer deleteRepo(t, "vmem-b")

	createVirtual(t, `{
		"name": "crud-virtual",
		"package_type": "helm",
		"members": ["vmem-a"]
	}`)
	defer deleteVirtual(t, "crud-virtual")

	// change members
	resp, body := doRequest(t, http.MethodPut, api("/api/v2/virtuals/crud-virtual"), []byte(`{
		"package_type": "helm",
		"members": ["vmem-a", "vmem-b"]
	}`), "application/json")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("update virtual: status %d: %s", resp.StatusCode, body)
	}

	resp, body = doRequest(t, http.MethodGet, api("/api/v2/virtuals/crud-virtual"), nil, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get virtual: status %d: %s", resp.StatusCode, body)
	}
	var v map[string]any
	if err := json.Unmarshal(body, &v); err != nil {
		t.Fatalf("decode virtual: %v", err)
	}
	members, _ := v["members"].([]any)
	if len(members) != 2 {
		t.Fatalf("expected 2 members after update, got %v", v["members"])
	}

	resp, _ = doRequest(t, http.MethodDelete, api("/api/v2/virtuals/crud-virtual"), nil, "")
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete virtual: status %d", resp.StatusCode)
	}
}

func getRepo(t *testing.T, name string) map[string]any {
	t.Helper()
	resp, body := doRequest(t, http.MethodGet, api("/api/v2/remotes/"+name), nil, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get remote %s: status %d: %s", name, resp.StatusCode, body)
	}
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatalf("decode remote %s: %v", name, err)
	}
	return m
}
