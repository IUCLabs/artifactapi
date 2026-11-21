package provider

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
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

func TestAssetStatusErrorClassification(t *testing.T) {
	for status, permanent := range map[int]bool{
		http.StatusNotFound: true, http.StatusGone: true, http.StatusRequestedRangeNotSatisfiable: true,
		http.StatusForbidden: false, http.StatusTooManyRequests: false, http.StatusInternalServerError: false, http.StatusBadGateway: false,
	} {
		err := AssetStatusError("u", &http.Response{StatusCode: status, Header: http.Header{}})
		if errors.Is(err, ErrInvalidPackage) != permanent {
			t.Errorf("status %d: permanent=%v, want %v", status, !permanent, permanent)
		}
	}
}

func TestReadPrefix(t *testing.T) {
	cases := []struct {
		name     string
		status   int
		cl       int64
		crange   string
		body     string
		n        int64
		wantFull bool
		wantErr  bool
	}{
		{name: "200 whole object", status: 200, cl: 5, body: "hello", n: 32, wantFull: true},
		{name: "200 larger than n", status: 200, cl: 5, body: "hello", n: 4},
		{name: "200 chunked shorter than n", status: 200, cl: -1, body: "hello", n: 32, wantFull: true},
		{name: "200 chunked larger than n", status: 200, cl: -1, body: "hello", n: 4},
		{name: "206 whole object", status: 206, crange: "bytes 0-4/5", body: "hello", n: 32, wantFull: true},
		{name: "206 prefix", status: 206, crange: "bytes 0-3/100", body: "hell", n: 4},
		{name: "206 short read", status: 206, crange: "bytes 0-31/100", body: "hello", n: 32, wantErr: true},
		{name: "206 no content-range short", status: 206, body: "hello", n: 32, wantErr: true},
		{name: "206 unknown total shorter than n", status: 206, cl: -1, crange: "bytes 0-4/*", body: "hello", n: 32, wantFull: true},
		{name: "206 unknown total prefix", status: 206, cl: -1, crange: "bytes 0-3/*", body: "hell", n: 4},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if tc.cl > 0 {
					w.Header().Set("Content-Length", strconv.FormatInt(tc.cl, 10))
				}
				if tc.crange != "" {
					w.Header().Set("Content-Range", tc.crange)
				}
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
				w.(http.Flusher).Flush()
			}))
			defer srv.Close()
			resp, err := http.Get(srv.URL)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = resp.Body.Close() }()
			if tc.cl == -1 && resp.ContentLength != -1 {
				t.Fatalf("response not chunked: ContentLength=%d", resp.ContentLength)
			}
			body, full, err := ReadPrefix(resp, tc.n)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err=%v, wantErr=%v", err, tc.wantErr)
			}
			if err != nil {
				if errors.Is(err, ErrInvalidPackage) {
					t.Fatalf("short read must be transient: %v", err)
				}
				return
			}
			if full != tc.wantFull || int64(len(body)) > tc.n {
				t.Fatalf("full=%v len=%d, want full=%v", full, len(body), tc.wantFull)
			}
		})
	}
}
