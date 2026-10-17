//go:build e2e

package e2e

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func getForwardedHTTPS(t *testing.T, url string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	req.Header.Set("X-Forwarded-Proto", "https")
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	return resp
}

func TestForwardedProtoCargoConfig(t *testing.T) {
	createRemote(t, `{"name": "scheme-cargo", "package_type": "cargo", "base_url": "https://index.crates.io"}`)
	defer deleteRemote(t, "scheme-cargo")

	resp := getForwardedHTTPS(t, apiURL("/api/v1/remote/scheme-cargo/config.json"))
	defer resp.Body.Close()
	var cfg map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&cfg); err != nil {
		t.Fatal(err)
	}
	if want := "https://" + strings.TrimPrefix(baseURL, "http://") + "/api/v1/remote/scheme-cargo/"; !strings.HasPrefix(cfg["dl"], want) {
		t.Errorf("dl = %q, want prefix %q", cfg["dl"], want)
	}
}

func TestForwardedProtoVirtualRedirect(t *testing.T) {
	createRemote(t, `{"name": "scheme-rpm", "package_type": "rpm", "base_url": "https://rpm.example.com"}`)
	defer deleteRemote(t, "scheme-rpm")
	resp, err := http.Post(apiURL("/api/v2/virtuals"), "application/json",
		strings.NewReader(`{"name": "scheme-virt", "package_type": "rpm", "members": ["scheme-rpm"]}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create virtual: status %d", resp.StatusCode)
	}
	defer deleteRequest(t, apiURL("/api/v2/virtuals/scheme-virt"))

	resp = getForwardedHTTPS(t, apiURL("/api/v1/virtual/scheme-virt/scheme-rpm/Packages/a.rpm"))
	resp.Body.Close()
	loc := resp.Header.Get("Location")
	if want := "https://" + strings.TrimPrefix(baseURL, "http://") + "/api/v1/remote/scheme-rpm/Packages/a.rpm"; resp.StatusCode != http.StatusFound || loc != want {
		t.Errorf("redirect = %d %q, want 302 %q", resp.StatusCode, loc, want)
	}
}
