package server

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5/middleware"
)

func TestRequestLogClientIP(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	h := middleware.ClientIPFromXFFTrustedProxies(1)(NewStructuredLogger()(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})))
	for _, tc := range []struct{ xff, want string }{
		{"6.6.6.6, 203.0.113.7", "remote=203.0.113.7"},
		{"", "remote=192.0.2.1 "},
	} {
		buf.Reset()
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		if tc.xff != "" {
			r.Header.Set("X-Forwarded-For", tc.xff)
		}
		h.ServeHTTP(httptest.NewRecorder(), r)
		if !strings.Contains(buf.String(), tc.want) {
			t.Errorf("xff %q: log %q missing %q", tc.xff, buf.String(), tc.want)
		}
	}
}
