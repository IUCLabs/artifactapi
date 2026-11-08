package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
	return b.require(next, "Bearer")
}

func (b *BearerOIDC) require(next http.Handler, challenge string) http.Handler {
	if b == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw := token(r)
		if raw == "" {
			unauthorized(w, challenge, "missing token")
			return
		}
		sub, err := b.verify(r.Context(), raw)
		if errors.Is(err, errIssuerUnavailable) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"error":"token issuer unavailable"}`))
			return
		}
		if err != nil {
			slog.Info("bearer token rejected", "error", err)
			unauthorized(w, challenge, "invalid token")
			return
		}
		slog.Info("bearer token accepted", "sub", sub, "method", r.Method, "path", r.URL.Path)
		next.ServeHTTP(w, r)
	})
}

func (b *BearerOIDC) verify(ctx context.Context, raw string) (string, error) {
	v, err := b.getVerifier(ctx)
	if err != nil {
		return "", errIssuerUnavailable
	}
	tok, err := v.Verify(ctx, raw)
	if err != nil {
		return "", err
	}
	if len(b.audiences) > 0 && !slices.ContainsFunc(tok.Audience, func(a string) bool { return slices.Contains(b.audiences, a) }) {
		return "", fmt.Errorf("audience %v not allowed", tok.Audience)
	}
	return tok.Subject, nil
}

// token reads the JWT from `Bearer`/`Token` credentials, the password of
// `Basic` credentials (any username), or a bare header value as `gem push`
// sends its API key.
func token(r *http.Request) string {
	if _, pass, ok := r.BasicAuth(); ok {
		return pass
	}
	scheme, raw, found := strings.Cut(strings.TrimSpace(r.Header.Get("Authorization")), " ")
	if !found {
		return scheme
	}
	if strings.EqualFold(scheme, "Bearer") || strings.EqualFold(scheme, "Token") {
		return strings.TrimSpace(raw)
	}
	return ""
}

// RequireWrites gates only mutating methods.
func (b *BearerOIDC) RequireWrites(next http.Handler) http.Handler {
	return writesOnly(next, b.Require(next))
}

// RequireRegistryWrites gates Docker Registry V2 writes behind the token flow
// Docker clients need: an unauthenticated ping gets a Bearer challenge naming
// realm, which serves the Basic password back as the token once verified, or
// an anonymous token so pulls stay open.
func (b *BearerOIDC) RequireRegistryWrites(next http.Handler, realm func(*http.Request) string) http.Handler {
	if b == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		challenge := `Bearer realm="` + realm(r) + `",service="artifactapi"`
		hasAuth := r.Header.Get("Authorization") != ""
		switch {
		case r.URL.Path == "/v2/token":
			writeToken(w, b.registryToken(w, r))
		case r.URL.Path == "/v2/" && !hasAuth:
			unauthorized(w, challenge, "authentication required")
		default:
			writesOnly(next, b.require(next, challenge)).ServeHTTP(w, r)
		}
	})
}

// registryToken echoes a valid credential back as the registry token. Missing
// or invalid credentials (e.g. an expired `docker login`) get the anonymous
// token so pulls keep working; writes are still verified per request.
// containerd/buildkit POST an OAuth2 password or refresh_token grant form;
// only a capped urlencoded body is read, never multipart.
func (b *BearerOIDC) registryToken(w http.ResponseWriter, r *http.Request) string {
	raw := token(r)
	if raw == "" && r.Method == http.MethodPost {
		r.Body = http.MaxBytesReader(w, r.Body, maxTokenForm)
		if r.ParseForm() == nil {
			if raw = r.PostForm.Get("password"); raw == "" {
				raw = r.PostForm.Get("refresh_token")
			}
		}
	}
	if raw == "" {
		return "anonymous"
	}
	if _, err := b.verify(r.Context(), raw); err != nil {
		slog.Info("registry credential rejected, issuing anonymous token", "error", err)
		return "anonymous"
	}
	return raw
}

// maxTokenForm fits a large OIDC JWT plus the grant's other fields.
const maxTokenForm = 16 << 10

func writeToken(w http.ResponseWriter, tok string) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"token": tok, "access_token": tok})
}

func writesOnly(next, gated http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
			next.ServeHTTP(w, r)
		default:
			gated.ServeHTTP(w, r)
		}
	})
}

func unauthorized(w http.ResponseWriter, challenge, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("WWW-Authenticate", challenge)
	w.WriteHeader(http.StatusUnauthorized)
	_, _ = w.Write([]byte(`{"error":"` + msg + `"}`))
}
