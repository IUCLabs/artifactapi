package provider

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"testing"
	"time"
)

func TestNewUpstreamStatusErrorRetryHint(t *testing.T) {
	reset := time.Now().Add(15 * time.Minute).Truncate(time.Second)
	cases := []struct {
		name   string
		header map[string]string
		want   time.Duration // 0 = no hint
	}{
		{"retry-after seconds", map[string]string{"Retry-After": "90"}, 90 * time.Second},
		{"retry-after date", map[string]string{"Retry-After": reset.UTC().Format(http.TimeFormat)}, 15 * time.Minute},
		{"quota exhausted", map[string]string{"X-RateLimit-Remaining": "0", "X-RateLimit-Reset": strconv.FormatInt(reset.Unix(), 10)}, 15 * time.Minute},
		{"quota left is not a hint", map[string]string{"X-RateLimit-Remaining": "12", "X-RateLimit-Reset": strconv.FormatInt(reset.Unix(), 10)}, 0},
		{"no headers", nil, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			resp := &http.Response{StatusCode: http.StatusForbidden, Header: http.Header{}}
			for k, v := range c.header {
				resp.Header.Set(k, v)
			}
			e := NewUpstreamStatusError("u", resp)
			if c.want == 0 {
				if !e.RetryAt.IsZero() {
					t.Fatalf("unexpected hint %v", e.RetryAt)
				}
				return
			}
			if d := time.Until(e.RetryAt) - c.want; d < -2*time.Second || d > 2*time.Second {
				t.Fatalf("retry in %v, want ~%v", time.Until(e.RetryAt), c.want)
			}
		})
	}
}

func TestNewSyncResult(t *testing.T) {
	if r := NewSyncResult(`"e"`, nil, time.Hour); r.Failed || r.Etag != `"e"` {
		t.Fatalf("success result = %+v", r)
	}

	r := NewSyncResult(`"e"`, errors.New("boom"), time.Hour)
	if !r.Failed || r.Backoff != time.Minute || r.MaxBackoff != 10*time.Minute || !r.RetryAt.IsZero() {
		t.Fatalf("plain failure = %+v, want 1m backoff capped at 10m, no hint", r)
	}
	if r := NewSyncResult("", errors.New("boom"), 5*time.Minute); r.MaxBackoff != time.Minute+15*time.Second {
		t.Fatalf("short ttl cap = %v, want ttl/4", r.MaxBackoff)
	}

	far := &UpstreamStatusError{Status: 403, RetryAt: time.Now().Add(3 * time.Hour)}
	r = NewSyncResult("", fmt.Errorf("scan: %w", far), time.Hour)
	if until := time.Until(r.RetryAt); until > time.Hour || until < 59*time.Minute {
		t.Fatalf("wrapped hint not clamped to ttl: retry in %v", until)
	}
}
