package v1

import (
	"crypto/tls"
	"net/http"
	"testing"
)

func TestScheme(t *testing.T) {
	if got := scheme(&http.Request{TLS: &tls.ConnectionState{}}); got != "https" {
		t.Errorf("TLS request scheme = %q, want https", got)
	}
	r := &http.Request{Header: http.Header{"X-Forwarded-Proto": {"https"}}}
	if got := scheme(r); got != "https" {
		t.Errorf("X-Forwarded-Proto scheme = %q, want https", got)
	}
	if got := scheme(&http.Request{Header: http.Header{}}); got != "http" {
		t.Errorf("default scheme = %q, want http", got)
	}
}
