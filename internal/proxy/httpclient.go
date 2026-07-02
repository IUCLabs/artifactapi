package proxy

import (
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/IUCLabs/artifactapi/pkg/models"
)

// Default upstream timeouts. A remote may override any of these; a zero
// override falls back to the default here. There is deliberately no overall
// Client.Timeout: the proxy streams arbitrarily large artifacts and total time
// is bounded by the request context instead. We only constrain the phases that
// must never hang — connect, TLS handshake, and time-to-first-response-header —
// so a slow or wedged upstream cannot pin a goroutine and connection.
const (
	defaultDialTimeout           = 10 * time.Second
	defaultTLSTimeout            = 10 * time.Second
	defaultResponseHeaderTimeout = 30 * time.Second
)

type clientKey struct {
	dial       time.Duration
	tls        time.Duration
	respHeader time.Duration
}

var (
	clientCacheMu sync.Mutex
	clientCache   = map[clientKey]*http.Client{}
)

// upstreamClientFor returns an HTTP client configured with the given timeouts,
// reusing a cached client (and its connection pool) for identical timeout sets.
// Zero values fall back to the defaults.
func upstreamClientFor(dial, tls, respHeader time.Duration) *http.Client {
	if dial <= 0 {
		dial = defaultDialTimeout
	}
	if tls <= 0 {
		tls = defaultTLSTimeout
	}
	if respHeader <= 0 {
		respHeader = defaultResponseHeaderTimeout
	}
	key := clientKey{dial: dial, tls: tls, respHeader: respHeader}

	clientCacheMu.Lock()
	defer clientCacheMu.Unlock()
	if c, ok := clientCache[key]; ok {
		return c
	}

	c := &http.Client{
		Transport: &http.Transport{
			Proxy: http.ProxyFromEnvironment,
			DialContext: (&net.Dialer{
				Timeout:   dial,
				KeepAlive: 30 * time.Second,
			}).DialContext,
			MaxIdleConns:          100,
			MaxIdleConnsPerHost:   10,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   tls,
			ExpectContinueTimeout: 1 * time.Second,
			ResponseHeaderTimeout: respHeader,
		},
	}
	clientCache[key] = c
	return c
}

// clientForRemote returns the upstream client for a remote, applying its
// per-remote timeout overrides (in seconds) on top of the defaults.
func clientForRemote(remote models.Remote) *http.Client {
	return upstreamClientFor(
		time.Duration(remote.UpstreamDialTimeout)*time.Second,
		time.Duration(remote.UpstreamTLSTimeout)*time.Second,
		time.Duration(remote.UpstreamResponseHeaderTimeout)*time.Second,
	)
}
