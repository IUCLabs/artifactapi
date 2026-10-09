package provider

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"
)

const (
	syncRetryBase = time.Minute
	syncRetryMax  = 10 * time.Minute
)

// UpstreamStatusError is a non-success upstream response. RetryAt is the
// upstream's own retry hint (Retry-After, or X-RateLimit-Reset once the quota
// is exhausted); zero when it gave none.
type UpstreamStatusError struct {
	URL     string
	Status  int
	RetryAt time.Time
}

func (e *UpstreamStatusError) Error() string {
	return fmt.Sprintf("%s: status %d", e.URL, e.Status)
}

// NewUpstreamStatusError wraps a non-success response, capturing its retry hint.
func NewUpstreamStatusError(url string, resp *http.Response) *UpstreamStatusError {
	e := &UpstreamStatusError{URL: url, Status: resp.StatusCode}
	if ra := resp.Header.Get("Retry-After"); ra != "" {
		if secs, err := strconv.Atoi(ra); err == nil {
			e.RetryAt = time.Now().Add(time.Duration(secs) * time.Second)
		} else if t, err := http.ParseTime(ra); err == nil {
			e.RetryAt = t
		}
	} else if resp.Header.Get("X-RateLimit-Remaining") == "0" {
		if reset, err := strconv.ParseInt(resp.Header.Get("X-RateLimit-Reset"), 10, 64); err == nil {
			e.RetryAt = time.Unix(reset, 0)
		}
	}
	return e
}

// SyncResult is a background scan's outcome, recorded when its sync lease is
// released. A failed scan keeps the prior sync time and ETag and schedules a
// retry after Backoff, doubled per consecutive failure up to MaxBackoff, and
// never earlier than RetryAt.
type SyncResult struct {
	Etag       string
	Failed     bool
	RetryAt    time.Time
	Backoff    time.Duration
	MaxBackoff time.Duration
}

// NewSyncResult builds the result for a scan against a remote with the given
// mutable_ttl. The retry cap stays well below ttl, and an upstream hint is
// clamped to ttl so a bogus reset can never stall the remote longer than a
// normal sync interval would.
func NewSyncResult(etag string, scanErr error, ttl time.Duration) SyncResult {
	if scanErr == nil {
		return SyncResult{Etag: etag}
	}
	res := SyncResult{Failed: true, Backoff: syncRetryBase, MaxBackoff: min(syncRetryMax, max(syncRetryBase, ttl/4))}
	var se *UpstreamStatusError
	if errors.As(scanErr, &se) && !se.RetryAt.IsZero() {
		res.RetryAt = se.RetryAt
		if limit := time.Now().Add(ttl); res.RetryAt.After(limit) {
			res.RetryAt = limit
		}
	}
	return res
}
