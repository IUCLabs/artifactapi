package v1

import (
	"crypto/tls"
	"net/http"
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
