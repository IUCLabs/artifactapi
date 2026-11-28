package keycrypt_test

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/IUCLabs/artifactapi/internal/keycrypt"
	"github.com/IUCLabs/artifactapi/internal/keycrypt/keycrypttest"
)

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func newTransit(t *testing.T, v *keycrypttest.Vault) (*keycrypt.Transit, *clock) {
	t.Helper()
	tr, err := keycrypt.NewTransit(v.Config())
	if err != nil {
		t.Fatal(err)
	}
	c := &clock{t: time.Unix(1_700_000_000, 0)}
	tr.SetClock(c.now)
	return tr, c
}

func TestTransitRoundTrip(t *testing.T) {
	v := keycrypttest.NewVault(t)
	tr, _ := newTransit(t, v)
	ctx := context.Background()
	stored, err := tr.Encrypt(ctx, "ci", []byte(pemText))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(stored, "enc:v1:transit:vault:v1:") || strings.Contains(stored, "PRIVATE KEY") {
		t.Fatalf("stored = %q", stored)
	}
	pt, err := tr.Decrypt(ctx, "ci", stored)
	if err != nil || string(pt) != pemText {
		t.Fatalf("decrypt = %q, %v", pt, err)
	}
	if got := v.Logins(); !slices.Equal(got, []string{"jwt-1"}) {
		t.Fatalf("logins = %v; want one login reusing the token", got)
	}
	if _, err := tr.Decrypt(ctx, "ci", "enc:v1:static:AAAA"); err == nil {
		t.Fatal("transit decrypted a static ciphertext")
	}
}

func TestTransitRenewsBeforeExpiryAndReloginsAfter(t *testing.T) {
	v := keycrypttest.NewVault(t)
	v.Set(func(v *keycrypttest.Vault) { v.Lease = 60 })
	tr, c := newTransit(t, v)
	ctx := context.Background()
	if _, err := tr.Encrypt(ctx, "ci", []byte("x")); err != nil {
		t.Fatal(err)
	}
	c.advance(30 * time.Second)
	_, _ = tr.Encrypt(ctx, "ci", []byte("x"))
	if renews, _ := v.Stats(); renews != 0 {
		t.Fatalf("renewed at half lease: %d", renews)
	}
	c.advance(15 * time.Second)
	if _, err := tr.Encrypt(ctx, "ci", []byte("x")); err != nil {
		t.Fatal(err)
	}
	if renews, _ := v.Stats(); renews != 1 || len(v.Logins()) != 1 {
		t.Fatalf("at 3/4 lease: %d renews, %d logins; want 1 renew, 1 login", renews, len(v.Logins()))
	}
	c.advance(61 * time.Second)
	if _, err := tr.Encrypt(ctx, "ci", []byte("x")); err != nil {
		t.Fatal(err)
	}
	if n := len(v.Logins()); n != 2 {
		t.Fatalf("after expiry: %d logins; want re-login", n)
	}

	v.Set(func(v *keycrypttest.Vault) { v.Renewable = false })
	v.Revoke()
	_, _ = tr.Encrypt(ctx, "ci", []byte("x"))
	c.advance(50 * time.Second)
	if _, err := tr.Encrypt(ctx, "ci", []byte("x")); err != nil {
		t.Fatal(err)
	}
	if renews, _ := v.Stats(); renews != 1 || len(v.Logins()) != 4 {
		t.Fatalf("non-renewable: %d renews, %d logins; want a fresh login instead of renew", renews, len(v.Logins()))
	}
}

func TestTransitReloginOn403RereadsJWT(t *testing.T) {
	v := keycrypttest.NewVault(t)
	tr, _ := newTransit(t, v)
	ctx := context.Background()
	stored, err := tr.Encrypt(ctx, "ci", []byte(pemText))
	if err != nil {
		t.Fatal(err)
	}
	v.Revoke()
	v.SetJWT(t, "jwt-2")
	pt, err := tr.Decrypt(ctx, "ci", stored)
	if err != nil || string(pt) != pemText {
		t.Fatalf("decrypt after revoke = %q, %v", pt, err)
	}
	if got := v.Logins(); !slices.Equal(got, []string{"jwt-1", "jwt-2"}) {
		t.Fatalf("logins = %v; want re-login with the rotated JWT", got)
	}
}

func TestTransitFailures(t *testing.T) {
	ctx := context.Background()
	v := keycrypttest.NewVault(t)
	cfg := v.Config()
	cfg.Role = "wrong"
	bad, _ := keycrypt.NewTransit(cfg)
	if err := bad.Check(ctx); err == nil || !strings.Contains(err.Error(), `role "wrong"`) {
		t.Fatalf("bad role: %v", err)
	}

	good, _ := newTransit(t, v)
	if err := good.Check(ctx); err != nil {
		t.Fatal(err)
	}
	v.Set(func(v *keycrypttest.Vault) { v.FailEncrypt, v.EchoErrors = true, true })
	_, err := good.Encrypt(ctx, "ci", []byte(pemText))
	if err == nil || strings.Contains(err.Error(), "MIIEsecret") {
		t.Fatalf("encrypt failure = %v; want an error without plaintext", err)
	}

	missing := v.Config()
	missing.TokenPath = filepath.Join(t.TempDir(), "absent")
	m, _ := keycrypt.NewTransit(missing)
	if _, err := m.Encrypt(ctx, "ci", []byte("x")); err == nil || !strings.Contains(err.Error(), "service account token") {
		t.Fatalf("missing JWT: %v", err)
	}

	ca := filepath.Join(t.TempDir(), "ca.pem")
	_ = os.WriteFile(ca, []byte("not a cert"), 0o600)
	withCA := v.Config()
	withCA.CACert = ca
	if _, err := keycrypt.NewTransit(withCA); err == nil {
		t.Fatal("accepted a CA file without certificates")
	}
}

func TestTransitPersistent403Fails(t *testing.T) {
	v := keycrypttest.NewVault(t)
	tr, _ := newTransit(t, v)
	ctx := context.Background()
	stored, _ := tr.Encrypt(ctx, "ci", []byte("x"))
	v.Set(func(v *keycrypttest.Vault) { v.DenyTransit = true })
	if _, err := tr.Decrypt(ctx, "ci", stored); err == nil || !strings.Contains(err.Error(), "HTTP 403") {
		t.Fatalf("err = %v; want HTTP 403", err)
	}
	if n := len(v.Logins()); n != 2 {
		t.Fatalf("%d logins; want exactly one re-login before giving up", n)
	}
}
