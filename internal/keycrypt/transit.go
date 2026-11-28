package keycrypt

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

// Transit encrypts with a derived Vault transit key, using the app name as
// the derivation context, authenticating via Kubernetes auth.
type Transit struct {
	cfg    VaultConfig
	client *http.Client
	now    func() time.Time

	mu        sync.Mutex
	token     string
	renewable bool
	lease     time.Duration
	issued    time.Time
}

// NewTransit builds a transit cipher; it does not contact Vault.
func NewTransit(cfg VaultConfig) (*Transit, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	if cfg.CACert != "" {
		pemBytes, err := os.ReadFile(cfg.CACert)
		if err != nil {
			return nil, fmt.Errorf("VAULT_CACERT: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pemBytes) {
			return nil, fmt.Errorf("VAULT_CACERT %s: no PEM certificates", cfg.CACert)
		}
		tr.TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	}
	cfg.Addr = strings.TrimRight(cfg.Addr, "/")
	client := &http.Client{
		Transport: tr,
		Timeout:   30 * time.Second,
		// A redirect would re-send X-Vault-Token to wherever it points.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	return &Transit{cfg: cfg, client: client, now: time.Now}, nil
}

func (t *Transit) Mode() string { return ModeTransit }

// Check logs in and round-trips a probe value, so a bad role, policy or key
// fails startup instead of the first write.
func (t *Transit) Check(ctx context.Context) error {
	const probe = "artifactapi-keycrypt-probe"
	ct, err := t.Encrypt(ctx, probe, []byte(probe))
	if err != nil {
		return err
	}
	pt, err := t.Decrypt(ctx, probe, ct)
	if err != nil {
		return err
	}
	if string(pt) != probe {
		return errors.New("vault transit probe round-trip mismatch")
	}
	return nil
}

func transitContext(name string) (string, error) {
	if name == "" {
		return "", errors.New("vault transit: empty app name")
	}
	return base64.StdEncoding.EncodeToString([]byte(name)), nil
}

func (t *Transit) Encrypt(ctx context.Context, name string, plaintext []byte) (string, error) {
	dctx, err := transitContext(name)
	if err != nil {
		return "", err
	}
	enc := make([]byte, base64.StdEncoding.EncodedLen(len(plaintext)))
	base64.StdEncoding.Encode(enc, plaintext)
	body := make([]byte, 0, len(enc)+len(dctx)+32)
	body = append(body, `{"plaintext":"`...)
	body = append(body, enc...)
	body = append(body, `","context":"`...)
	body = append(body, dctx...)
	body = append(body, `"}`...)
	Zero(enc)
	resp, err := t.call(ctx, "encrypt", body)
	Zero(body)
	if err != nil {
		return "", err
	}
	var out struct {
		Data struct {
			Ciphertext string `json:"ciphertext"`
		} `json:"data"`
	}
	if err := json.Unmarshal(resp, &out); err != nil || !strings.HasPrefix(out.Data.Ciphertext, "vault:") {
		return "", errors.New("vault transit encrypt: unexpected response")
	}
	return transitPrefix + out.Data.Ciphertext, nil
}

func (t *Transit) Decrypt(ctx context.Context, name, stored string) ([]byte, error) {
	if !strings.HasPrefix(stored, transitPrefix) {
		return nil, mismatch(stored, ModeTransit)
	}
	dctx, err := transitContext(name)
	if err != nil {
		return nil, err
	}
	body, err := json.Marshal(map[string]string{"ciphertext": strings.TrimPrefix(stored, transitPrefix), "context": dctx})
	if err != nil {
		return nil, err
	}
	resp, err := t.call(ctx, "decrypt", body)
	if err != nil {
		return nil, err
	}
	defer Zero(resp)
	var out struct {
		Data struct {
			Plaintext json.RawMessage `json:"plaintext"`
		} `json:"data"`
	}
	err = json.Unmarshal(resp, &out)
	raw := out.Data.Plaintext
	defer Zero(raw)
	if err != nil || len(raw) < 2 || raw[0] != '"' || raw[len(raw)-1] != '"' {
		return nil, errors.New("vault transit decrypt: unexpected response")
	}
	raw = raw[1 : len(raw)-1]
	pt := make([]byte, base64.StdEncoding.DecodedLen(len(raw)))
	n, err := base64.StdEncoding.Decode(pt, raw)
	if err != nil {
		Zero(pt)
		return nil, errors.New("vault transit decrypt: malformed plaintext")
	}
	return pt[:n], nil
}

func (t *Transit) call(ctx context.Context, op string, body []byte) ([]byte, error) {
	path := "/v1/" + strings.Trim(t.cfg.TransitMount, "/") + "/" + op + "/" + url.PathEscape(t.cfg.TransitKey)
	for attempt := 0; ; attempt++ {
		token, err := t.ensureToken(ctx)
		if err != nil {
			return nil, err
		}
		status, resp, err := t.post(ctx, path, token, body)
		if err != nil {
			return nil, fmt.Errorf("vault transit %s: %w", op, err)
		}
		if status == http.StatusForbidden && attempt == 0 {
			Zero(resp)
			t.invalidate(token)
			continue
		}
		if status != http.StatusOK {
			msg := ""
			if op != "encrypt" {
				msg = vaultErrors(resp)
			}
			Zero(resp)
			return nil, fmt.Errorf("vault transit %s: HTTP %d%s", op, status, msg)
		}
		return resp, nil
	}
}

func (t *Transit) invalidate(token string) {
	t.mu.Lock()
	if t.token == token {
		t.token = ""
	}
	t.mu.Unlock()
}

const authTimeout = 10 * time.Second

func (t *Transit) ensureToken(ctx context.Context) (string, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.token != "" {
		if t.lease == 0 {
			return t.token, nil
		}
		age := t.now().Sub(t.issued)
		if age < t.lease*2/3 {
			return t.token, nil
		}
		if age < t.lease && t.renewable {
			if err := t.renewLocked(ctx); err == nil {
				return t.token, nil
			}
		}
		t.token = ""
	}
	if err := t.loginLocked(ctx); err != nil {
		return "", err
	}
	return t.token, nil
}

type vaultAuth struct {
	Auth *struct {
		ClientToken   string `json:"client_token"`
		LeaseDuration int64  `json:"lease_duration"`
		Renewable     bool   `json:"renewable"`
	} `json:"auth"`
}

func (t *Transit) loginLocked(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, authTimeout)
	defer cancel()
	jwt, err := os.ReadFile(t.cfg.TokenPath)
	if err != nil {
		return fmt.Errorf("vault kubernetes login: read service account token: %w", err)
	}
	body, err := json.Marshal(map[string]string{"role": t.cfg.Role, "jwt": string(bytes.TrimSpace(jwt))})
	Zero(jwt)
	if err != nil {
		return err
	}
	status, resp, err := t.post(ctx, "/v1/auth/"+strings.Trim(t.cfg.K8sMount, "/")+"/login", "", body)
	Zero(body)
	if err != nil {
		return fmt.Errorf("vault kubernetes login: %w", err)
	}
	if status != http.StatusOK {
		return fmt.Errorf("vault kubernetes login (role %q): HTTP %d%s", t.cfg.Role, status, vaultErrors(resp))
	}
	return t.setAuth(resp, "vault kubernetes login")
}

func (t *Transit) renewLocked(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, authTimeout)
	defer cancel()
	status, resp, err := t.post(ctx, "/v1/auth/token/renew-self", t.token, []byte(`{}`))
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return fmt.Errorf("vault token renew: HTTP %d", status)
	}
	return t.setAuth(resp, "vault token renew")
}

func (t *Transit) setAuth(resp []byte, op string) error {
	var out vaultAuth
	if err := json.Unmarshal(resp, &out); err != nil || out.Auth == nil || out.Auth.ClientToken == "" {
		return fmt.Errorf("%s: response has no client token", op)
	}
	t.token = out.Auth.ClientToken
	t.renewable = out.Auth.Renewable
	t.lease = time.Duration(out.Auth.LeaseDuration) * time.Second
	t.issued = t.now()
	return nil
}

func (t *Transit) post(ctx context.Context, path, token string, body []byte) (int, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.cfg.Addr+path, bytes.NewReader(body))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("X-Vault-Token", token)
	}
	resp, err := t.client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return 0, nil, err
	}
	return resp.StatusCode, b, nil
}

func vaultErrors(resp []byte) string {
	var out struct {
		Errors []string `json:"errors"`
	}
	if json.Unmarshal(resp, &out) != nil || len(out.Errors) == 0 {
		return ""
	}
	msg := strings.Join(out.Errors, "; ")
	if len(msg) > 200 {
		msg = msg[:200]
	}
	return ": " + msg
}
