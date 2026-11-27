//go:build e2e

package e2e

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
)

func findListener() (net.Listener, error) {
	return net.Listen("tcp", "127.0.0.1:0")
}

func apiURL(path string) string {
	return baseURL + path
}

func createRemote(t *testing.T, body string) {
	t.Helper()
	resp, err := http.Post(apiURL("/api/v2/remotes"), "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("create remote: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("create remote: status %d: %s", resp.StatusCode, b)
	}
}

func deleteRemote(t *testing.T, name string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodDelete, apiURL("/api/v2/remotes/"+name), nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("delete remote: %v", err)
	}
	resp.Body.Close()
}

func getJSON(t *testing.T, url string) map[string]any {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("GET %s: status %d: %s", url, resp.StatusCode, b)
	}
	var result map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return result
}

func getBody(t *testing.T, url string) ([]byte, int) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return b, resp.StatusCode
}

func getString(t *testing.T, url string) string {
	t.Helper()
	b, status := getBody(t, url)
	if status != http.StatusOK {
		t.Fatalf("GET %s: status %d: %s", url, status, b)
	}
	return string(b)
}

func assertStatus(t *testing.T, url string, wantStatus int) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	resp.Body.Close()
	if resp.StatusCode != wantStatus {
		t.Errorf("GET %s: got %d, want %d", url, resp.StatusCode, wantStatus)
	}
}

func deleteRequest(t *testing.T, url string) int {
	t.Helper()
	req, _ := http.NewRequest(http.MethodDelete, url, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("DELETE %s: %v", url, err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

func mustFormat(format string, args ...any) string {
	return fmt.Sprintf(format, args...)
}
