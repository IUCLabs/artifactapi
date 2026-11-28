package requrl

import (
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestScheme(t *testing.T) {
	if got := Scheme(&http.Request{TLS: &tls.ConnectionState{}}); got != "https" {
		t.Errorf("TLS request scheme = %q, want https", got)
	}
	r := &http.Request{Header: http.Header{"X-Forwarded-Proto": {"https"}}}
	if got := Scheme(r); got != "https" {
		t.Errorf("X-Forwarded-Proto scheme = %q, want https", got)
	}
	for fwd, want := range map[string]string{
		"https, http": "https",
		"http, https": "http",
		"HTTPS":       "https",
		" https ":     "https",
		"javascript":  "http",
		"ftp":         "http",
		"https;evil":  "http",
	} {
		r := &http.Request{Header: http.Header{"X-Forwarded-Proto": {fwd}}}
		if got := Scheme(r); got != want {
			t.Errorf("X-Forwarded-Proto %q scheme = %q, want %s", fwd, got, want)
		}
	}
	if got := Scheme(&http.Request{Header: http.Header{}}); got != "http" {
		t.Errorf("default scheme = %q, want http", got)
	}
}

func TestValidHost(t *testing.T) {
	for _, h := range []string{"aa.example", "aa.example:8443", "artifactapi.example.com", "192.0.2.1:80", "[::1]:8080", "[::1]", "::1", "localhost", "my_host.local"} {
		if !ValidHost(h) {
			t.Errorf("ValidHost(%q) = false, want true", h)
		}
	}
	for _, h := range []string{"", "evil\".example", "a b", "aa.example:0", "aa.example:99999", "aa.example:x", "aa.example:", ":8080", "-bad.example", "a..b", "[::1", "[nope]:80", "a/b", "a<script>", strings.Repeat("a", 64) + ".example"} {
		if ValidHost(h) {
			t.Errorf("ValidHost(%q) = true, want false", h)
		}
	}
}

func TestBaseURL(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Host = "aa.example:8443"
	r.Header.Set("X-Forwarded-Proto", "https")
	w := httptest.NewRecorder()
	if got, ok := BaseURL(w, r); !ok || got != "https://aa.example:8443" {
		t.Errorf("BaseURL = %q, %v; want https://aa.example:8443", got, ok)
	}
	r.Host = `evil"},{"x":"`
	w = httptest.NewRecorder()
	if got, ok := BaseURL(w, r); ok || got != "" || w.Code != http.StatusBadRequest {
		t.Errorf("invalid host: got %q ok=%v code=%d, want rejected with 400", got, ok, w.Code)
	}
}
