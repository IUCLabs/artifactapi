//go:build e2e

package e2e

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestHealth(t *testing.T) {
	result := getJSON(t, apiURL("/health"))
	if result["status"] != "ok" {
		t.Errorf("expected ok, got %v", result["status"])
	}
}

func TestRoot(t *testing.T) {
	result := getJSON(t, apiURL("/"))
	if result["name"] != "artifactapi" {
		t.Errorf("expected artifactapi, got %v", result["name"])
	}
}

func TestRemoteCRUD(t *testing.T) {
	createRemote(t, `{
		"name": "test-generic",
		"package_type": "generic",
		"base_url": "https://example.com",
		"description": "test remote",
		"mutable_ttl": 600,
		"check_mutable": true,
		"stale_on_error": true
	}`)
	defer deleteRemote(t, "test-generic")

	remote := getJSON(t, apiURL("/api/v2/remotes/test-generic"))
	if remote["name"] != "test-generic" {
		t.Errorf("expected test-generic, got %v", remote["name"])
	}
	if remote["package_type"] != "generic" {
		t.Errorf("expected generic, got %v", remote["package_type"])
	}

	req, _ := http.NewRequest(http.MethodPut, apiURL("/api/v2/remotes/test-generic"),
		strings.NewReader(`{
			"package_type": "generic",
			"base_url": "https://updated.example.com",
			"description": "updated",
			"mutable_ttl": 300,
			"check_mutable": true,
			"stale_on_error": true
		}`))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("update: status %d", resp.StatusCode)
	}

	updated := getJSON(t, apiURL("/api/v2/remotes/test-generic"))
	if updated["base_url"] != "https://updated.example.com" {
		t.Errorf("expected updated URL, got %v", updated["base_url"])
	}

	status := deleteRequest(t, apiURL("/api/v2/remotes/test-generic"))
	if status != http.StatusNoContent {
		t.Errorf("delete: got %d, want 204", status)
	}

	assertStatus(t, apiURL("/api/v2/remotes/test-generic"), http.StatusNotFound)
}

func TestRemoteList(t *testing.T) {
	createRemote(t, `{"name":"list-a","package_type":"generic","base_url":"https://a.example.com","stale_on_error":true}`)
	createRemote(t, `{"name":"list-b","package_type":"helm","base_url":"https://b.example.com","stale_on_error":true}`)
	defer deleteRemote(t, "list-a")
	defer deleteRemote(t, "list-b")

	resp, err := http.Get(apiURL("/api/v2/remotes"))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	var remotes []map[string]any
	json.Unmarshal(body, &remotes)

	if len(remotes) < 2 {
		t.Fatalf("expected at least 2 remotes, got %d", len(remotes))
	}
}

func TestVirtualCRUD(t *testing.T) {
	createRemote(t, `{"name":"virt-member-a","package_type":"helm","base_url":"https://a.example.com","stale_on_error":true}`)
	createRemote(t, `{"name":"virt-member-b","package_type":"helm","base_url":"https://b.example.com","stale_on_error":true}`)
	defer deleteRemote(t, "virt-member-a")
	defer deleteRemote(t, "virt-member-b")

	resp, err := http.Post(apiURL("/api/v2/virtuals"), "application/json",
		strings.NewReader(`{
			"name": "test-virtual",
			"package_type": "helm",
			"description": "test virtual",
			"members": ["virt-member-a", "virt-member-b"]
		}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create virtual: status %d", resp.StatusCode)
	}

	virt := getJSON(t, apiURL("/api/v2/virtuals/test-virtual"))
	if virt["name"] != "test-virtual" {
		t.Errorf("expected test-virtual, got %v", virt["name"])
	}

	status := deleteRequest(t, apiURL("/api/v2/virtuals/test-virtual"))
	if status != http.StatusNoContent {
		t.Errorf("delete virtual: got %d, want 204", status)
	}
}

func TestStatsEndpoint(t *testing.T) {
	result := getJSON(t, apiURL("/api/v2/stats"))
	if _, ok := result["total_remotes"]; !ok {
		t.Error("expected total_remotes in stats")
	}
}

func TestHealthV2(t *testing.T) {
	result := getJSON(t, apiURL("/api/v2/health"))
	if result["status"] != "ok" {
		t.Errorf("expected ok, got %v", result["status"])
	}
	if result["postgres"] != "ok" {
		t.Errorf("expected postgres ok, got %v", result["postgres"])
	}
}

func TestInvalidPackageType(t *testing.T) {
	resp, err := http.Post(apiURL("/api/v2/remotes"), "application/json",
		strings.NewReader(`{"name":"bad","package_type":"bogus","base_url":"https://x.com"}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("expected 400 for invalid package type, got %d", resp.StatusCode)
	}
}
