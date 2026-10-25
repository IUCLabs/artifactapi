package v1

import (
	"net/http/httptest"
	"testing"

	"github.com/IUCLabs/artifactapi/internal/proxy"
)

func TestWriteProxyErrorRetryAfter(t *testing.T) {
	w := httptest.NewRecorder()
	writeProxyError(w, &proxy.ProxyError{Status: 503, Message: "quarantine check unavailable", RetryAfter: 30})
	if w.Code != 503 || w.Header().Get("Retry-After") != "30" {
		t.Errorf("code %d, Retry-After %q", w.Code, w.Header().Get("Retry-After"))
	}
}
