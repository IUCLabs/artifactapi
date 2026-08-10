// Package githubauth provides the process-wide GitHub machine credential used to
// authenticate every outbound GitHub request (releases scan, ranged asset header
// fetches, and the generic-github byte proxy for private assets). The credential
// is delivered via env/secret only — it is never stored per-remote in the DB,
// never returned by any API, and never logged.
package githubauth

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
)

// Credential yields a bearer token for GitHub requests. Token may block to mint
// or refresh (the GitHub App path); an empty string means "no auth", which only
// happens when no credential is configured.
type Credential interface {
	Token(ctx context.Context) (string, error)
}

// Options is the raw, env-sourced auth configuration. Exactly one mode may be
// configured: a static token, or a GitHub App (id + installation id + private
// key). Partial App configuration is an error (fail closed); no fields at all is
// fine and yields a nil credential (anonymous, current behavior).
type Options struct {
	// Token is a Personal Access Token (fine-grained or classic) sent verbatim
	// as "Authorization: Bearer <token>".
	Token string

	// GitHub App fields. PrivateKeyPEM and PrivateKeyPath are alternatives; the
	// inline PEM wins when both are set.
	AppID          string
	InstallationID string
	PrivateKeyPEM  string
	PrivateKeyPath string

	// apiBaseURL overrides https://api.github.com for tests. Empty uses the real
	// endpoint. httpClient likewise overrides the default client for tests.
	apiBaseURL string
	httpClient httpDoer
}

// New builds the process credential from options, validating that auth is either
// fully configured or fully absent. It returns (nil, nil) when nothing is set.
func New(opts Options) (Credential, error) {
	hasToken := opts.Token != ""
	hasAppField := opts.AppID != "" || opts.InstallationID != "" ||
		opts.PrivateKeyPEM != "" || opts.PrivateKeyPath != ""

	switch {
	case !hasToken && !hasAppField:
		return nil, nil // no auth configured — anonymous is fine
	case hasToken && hasAppField:
		return nil, errors.New("github auth: both a token and GitHub App fields are set; configure exactly one")
	case hasToken:
		return staticToken{token: opts.Token}, nil
	default:
		return newAppCredential(opts)
	}
}

// staticToken is a fixed PAT credential.
type staticToken struct{ token string }

func (s staticToken) Token(context.Context) (string, error) { return s.token, nil }

// server is the process-wide credential set once at startup. A nil value means
// no server credential (anonymous). Access is guarded so a late SetServer in a
// test is race-free.
var (
	serverMu sync.RWMutex
	server   Credential
)

// SetServer installs the process credential. Call once during startup.
func SetServer(c Credential) {
	serverMu.Lock()
	server = c
	serverMu.Unlock()
}

// Server returns the process credential, or nil if none is configured.
func Server() Credential {
	serverMu.RLock()
	defer serverMu.RUnlock()
	return server
}

// loadPrivateKeyPEM resolves the App private key bytes from the inline PEM or a
// file path, without ever returning the key material in an error message.
func loadPrivateKeyPEM(opts Options) ([]byte, error) {
	if strings.TrimSpace(opts.PrivateKeyPEM) != "" {
		return []byte(opts.PrivateKeyPEM), nil
	}
	if opts.PrivateKeyPath != "" {
		b, err := os.ReadFile(opts.PrivateKeyPath)
		if err != nil {
			return nil, fmt.Errorf("github app: read private key file: %w", err)
		}
		return b, nil
	}
	return nil, errors.New("github app: no private key configured")
}
