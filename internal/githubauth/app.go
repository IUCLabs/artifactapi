package githubauth

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

const (
	defaultAPIBase = "https://api.github.com"

	// jwtLifetime is how long the app JWT is valid. GitHub caps it at 10 minutes;
	// 9 leaves headroom for clock skew.
	jwtLifetime = 9 * time.Minute
	// jwtBackdate backdates iat to tolerate the app server's clock running behind
	// GitHub's, which otherwise rejects the JWT.
	jwtBackdate = 60 * time.Second
	// refreshSkew refreshes the installation token this long before it expires so
	// a request never races an expiry.
	refreshSkew = 5 * time.Minute
)

type httpDoer interface {
	Do(*http.Request) (*http.Response, error)
}

// appCredential mints installation access tokens for a GitHub App. It signs a
// short-lived RS256 JWT with the app private key, exchanges it for a ~1h
// installation token, caches that token, and refreshes it shortly before expiry.
// Refreshes are single-flighted by holding the mutex across the exchange, so
// concurrent callers coalesce onto one HTTP request and reuse the cached token.
type appCredential struct {
	appID          string
	installationID string
	key            *rsa.PrivateKey
	apiBase        string
	client         httpDoer

	mu     sync.Mutex
	token  string
	expiry time.Time
}

func newAppCredential(opts Options) (*appCredential, error) {
	if opts.AppID == "" {
		return nil, errors.New("github app: GITHUB_APP_ID is required")
	}
	if opts.InstallationID == "" {
		return nil, errors.New("github app: GITHUB_APP_INSTALLATION_ID is required")
	}
	pemBytes, err := loadPrivateKeyPEM(opts)
	if err != nil {
		return nil, err
	}
	key, err := parseRSAPrivateKey(pemBytes)
	if err != nil {
		return nil, err
	}

	apiBase := opts.apiBaseURL
	if apiBase == "" {
		apiBase = defaultAPIBase
	}
	client := opts.httpClient
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}

	return &appCredential{
		appID:          opts.AppID,
		installationID: opts.InstallationID,
		key:            key,
		apiBase:        strings.TrimRight(apiBase, "/"),
		client:         client,
	}, nil
}

// Token returns a cached installation token, refreshing it under a single-flight
// lock when it is missing or within refreshSkew of expiry.
func (a *appCredential) Token(ctx context.Context) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.token != "" && time.Now().Before(a.expiry.Add(-refreshSkew)) {
		return a.token, nil
	}
	if err := a.refreshLocked(ctx); err != nil {
		return "", err
	}
	return a.token, nil
}

func (a *appCredential) refreshLocked(ctx context.Context) error {
	jwt, err := mintJWT(a.appID, a.key, time.Now())
	if err != nil {
		return fmt.Errorf("github app: mint jwt: %w", err)
	}

	u := fmt.Sprintf("%s/app/installations/%s/access_tokens", a.apiBase, a.installationID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+jwt)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	resp, err := a.client.Do(req)
	if err != nil {
		return fmt.Errorf("github app: token exchange: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		// Never echo the body verbatim — it can contain sensitive material.
		return fmt.Errorf("github app: token exchange status %d", resp.StatusCode)
	}

	var out struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return fmt.Errorf("github app: decode token response: %w", err)
	}
	if out.Token == "" {
		return errors.New("github app: token exchange returned an empty token")
	}
	a.token = out.Token
	a.expiry = out.ExpiresAt
	if a.expiry.IsZero() {
		// Defensive: assume the documented ~1h lifetime if GitHub omits it.
		a.expiry = time.Now().Add(time.Hour)
	}
	return nil
}

// mintJWT builds and RS256-signs a GitHub App JWT (iss=app id, backdated iat,
// ≤10m exp) using stdlib crypto — no third-party JWT dependency.
func mintJWT(appID string, key *rsa.PrivateKey, now time.Time) (string, error) {
	header := map[string]string{"alg": "RS256", "typ": "JWT"}
	claims := map[string]any{
		"iat": now.Add(-jwtBackdate).Unix(),
		"exp": now.Add(jwtLifetime).Unix(),
		"iss": appID,
	}
	hb, err := json.Marshal(header)
	if err != nil {
		return "", err
	}
	cb, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	signingInput := b64url(hb) + "." + b64url(cb)
	digest := sha256.Sum256([]byte(signingInput))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	if err != nil {
		return "", err
	}
	return signingInput + "." + b64url(sig), nil
}

func b64url(b []byte) string {
	return base64.RawURLEncoding.EncodeToString(b)
}

// parseRSAPrivateKey accepts PKCS#1 ("RSA PRIVATE KEY") and PKCS#8 ("PRIVATE
// KEY") PEM, covering both GitHub App key export formats.
func parseRSAPrivateKey(pemBytes []byte) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return nil, errors.New("github app: private key is not valid PEM")
	}
	if key, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return key, nil
	}
	keyAny, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, errors.New("github app: private key is not a supported RSA PKCS#1/PKCS#8 key")
	}
	rsaKey, ok := keyAny.(*rsa.PrivateKey)
	if !ok {
		return nil, errors.New("github app: private key is not an RSA key")
	}
	return rsaKey, nil
}
