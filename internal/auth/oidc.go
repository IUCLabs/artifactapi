package auth

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
)

// BearerOIDC verifies `Authorization: Bearer <jwt>` against an OIDC issuer's
// JWKS. Discovery is lazy so startup never depends on the issuer being
// reachable; a failed discovery is not retried until the backoff elapses.
type BearerOIDC struct {
	issuer    string
	audiences []string
	timeout   time.Duration
	backoff   time.Duration
	discover  func(context.Context, string) (*oidc.Provider, error)

	mu       sync.Mutex
	verifier *oidc.IDTokenVerifier
	inflight chan struct{}
	failedAt time.Time
}

var errIssuerUnavailable = errors.New("oidc issuer unavailable")

// NewBearerOIDC returns nil when issuer is empty, meaning auth is disabled.
func NewBearerOIDC(issuer string, audiences []string) *BearerOIDC {
	if issuer == "" {
		return nil
	}
	return &BearerOIDC{issuer: issuer, audiences: audiences, timeout: 10 * time.Second, backoff: 15 * time.Second, discover: oidc.NewProvider}
}

// getVerifier runs at most one discovery at a time without holding the lock
// across network I/O; concurrent callers wait for it, and only callers within
// the post-failure backoff fail fast.
func (b *BearerOIDC) getVerifier(ctx context.Context) (*oidc.IDTokenVerifier, error) {
	b.mu.Lock()
	if v := b.verifier; v != nil {
		b.mu.Unlock()
		return v, nil
	}
	if done := b.inflight; done != nil {
		b.mu.Unlock()
		select {
		case <-done:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		b.mu.Lock()
		defer b.mu.Unlock()
		if b.verifier == nil {
			return nil, errIssuerUnavailable
		}
		return b.verifier, nil
	}
	if time.Since(b.failedAt) < b.backoff {
		b.mu.Unlock()
		return nil, errIssuerUnavailable
	}
	done := make(chan struct{})
	b.inflight = done
	b.mu.Unlock()

	v, err := b.discoverOnce(done)
	if err != nil {
		slog.Error("oidc discovery failed", "issuer", b.issuer, "error", err)
		return nil, err
	}
	return v, nil
}

// discoverOnce releases inflight and waiters on every exit, a panic included,
// recording failedAt unless discovery succeeded.
func (b *BearerOIDC) discoverOnce(done chan struct{}) (v *oidc.IDTokenVerifier, err error) {
	defer func() {
		b.mu.Lock()
		b.inflight = nil
		if v == nil {
			b.failedAt = time.Now()
		}
		b.verifier = v
		b.mu.Unlock()
		close(done)
	}()
	// The provider keeps this context and client for JWKS refreshes, so the
	// deadline lives on the client rather than the context.
	client := &http.Client{Timeout: b.timeout}
	p, err := b.discover(oidc.ClientContext(context.Background(), client), b.issuer)
	if err != nil {
		return nil, err
	}
	return p.Verifier(&oidc.Config{SkipClientIDCheck: true}), nil
}

// Require gates every request through next on a valid token. A nil receiver
// passes everything through.
func (b *BearerOIDC) Require(next http.Handler) http.Handler {
	if b == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		scheme, raw, _ := strings.Cut(strings.TrimSpace(r.Header.Get("Authorization")), " ")
		raw = strings.TrimSpace(raw)
		if !strings.EqualFold(scheme, "Bearer") || raw == "" {
			unauthorized(w, "missing bearer token")
			return
		}
		v, err := b.getVerifier(r.Context())
		if err != nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"error":"token issuer unavailable"}`))
			return
		}
		tok, err := v.Verify(r.Context(), raw)
		if err != nil {
			slog.Info("bearer token rejected", "error", err)
			unauthorized(w, "invalid token")
			return
		}
		if len(b.audiences) > 0 && !slices.ContainsFunc(tok.Audience, func(a string) bool { return slices.Contains(b.audiences, a) }) {
			slog.Info("bearer token rejected", "error", "audience not allowed", "aud", tok.Audience)
			unauthorized(w, "invalid token")
			return
		}
		slog.Info("bearer token accepted", "sub", tok.Subject, "method", r.Method, "path", r.URL.Path)
		next.ServeHTTP(w, r)
	})
}

// RequireWrites gates only mutating methods.
func (b *BearerOIDC) RequireWrites(next http.Handler) http.Handler {
	gated := b.Require(next)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
			next.ServeHTTP(w, r)
		default:
			gated.ServeHTTP(w, r)
		}
	})
}

func unauthorized(w http.ResponseWriter, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("WWW-Authenticate", "Bearer")
	w.WriteHeader(http.StatusUnauthorized)
	_, _ = w.Write([]byte(`{"error":"` + msg + `"}`))
}
