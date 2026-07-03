//go:build dockere2e

// Package e2edocker holds the black-box end-to-end suite that runs against a
// fully dockerised artifactapi stack (see scripts/docker-e2e.sh). Unlike the
// in-process e2e suite, these tests only speak HTTP to the running container.
package e2edocker

import (
	"bytes"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func baseURL() string {
	if v := os.Getenv("ARTIFACTAPI_URL"); v != "" {
		return strings.TrimRight(v, "/")
	}
	return "http://localhost:8000"
}

// mockUpstream is the base URL the artifactapi *container* uses to reach the
// static mock upstream. It is resolved on the compose network, not the host.
func mockUpstream() string {
	if v := os.Getenv("MOCK_UPSTREAM_INTERNAL"); v != "" {
		return strings.TrimRight(v, "/")
	}
	return "http://mockupstream"
}

func api(path string) string { return baseURL() + path }

func fixtureBytes(t *testing.T, rel string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("fixtures", rel))
	if err != nil {
		t.Fatalf("read fixture %s: %v", rel, err)
	}
	return b
}

func doRequest(t *testing.T, method, url string, body []byte, contentType string) (*http.Response, []byte) {
	t.Helper()
	var r io.Reader
	if body != nil {
		r = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, url, r)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	return resp, respBody
}

func createRepo(t *testing.T, jsonBody string) {
	t.Helper()
	resp, body := doRequest(t, http.MethodPost, api("/api/v2/remotes"), []byte(jsonBody), "application/json")
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create repo: status %d: %s", resp.StatusCode, body)
	}
}

func deleteRepo(t *testing.T, name string) {
	t.Helper()
	doRequest(t, http.MethodDelete, api("/api/v2/remotes/"+name), nil, "")
}

func createVirtual(t *testing.T, jsonBody string) {
	t.Helper()
	resp, body := doRequest(t, http.MethodPost, api("/api/v2/virtuals"), []byte(jsonBody), "application/json")
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create virtual: status %d: %s", resp.StatusCode, body)
	}
}

func deleteVirtual(t *testing.T, name string) {
	t.Helper()
	doRequest(t, http.MethodDelete, api("/api/v2/virtuals/"+name), nil, "")
}

// getEventually retries a GET until it returns 200 or the deadline passes. Used
// for asynchronously-generated artifacts (e.g. rpm repodata after upload).
func getEventually(t *testing.T, url string, timeout time.Duration) (*http.Response, []byte) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var resp *http.Response
	var body []byte
	for {
		resp, body = doRequest(t, http.MethodGet, url, nil, "")
		if resp.StatusCode == http.StatusOK || time.Now().After(deadline) {
			return resp, body
		}
		time.Sleep(250 * time.Millisecond)
	}
}
