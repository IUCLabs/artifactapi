package keycrypt_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"strings"
	"testing"

	"github.com/IUCLabs/artifactapi/internal/keycrypt"
)

const pemText = "-----BEGIN RSA PRIVATE KEY-----\nMIIEsecret\n-----END RSA PRIVATE KEY-----\n"

func staticKey(b byte) []byte { return bytes.Repeat([]byte{b}, 32) }

func newStatic(t *testing.T, b byte) *keycrypt.Static {
	t.Helper()
	s, err := keycrypt.NewStatic(staticKey(b))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestStaticRoundTrip(t *testing.T) {
	s := newStatic(t, 1)
	ctx := context.Background()
	a, err := s.Encrypt(ctx, "ci", []byte(pemText))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := s.Encrypt(ctx, "ci", []byte(pemText))
	if !strings.HasPrefix(a, "enc:v1:static:") || strings.Contains(a, "PRIVATE KEY") || a == b {
		t.Fatalf("ciphertexts %q / %q: want prefixed, opaque, fresh nonce per value", a, b)
	}
	raw, _ := base64.StdEncoding.DecodeString(strings.TrimPrefix(a, "enc:v1:static:"))
	if len(raw) != 12+len(pemText)+16 {
		t.Fatalf("sealed length %d; want nonce(12)+pt+tag(16)", len(raw))
	}
	pt, err := s.Decrypt(ctx, "ci", a)
	if err != nil || string(pt) != pemText {
		t.Fatalf("decrypt = %q, %v", pt, err)
	}
	if keycrypt.IsLegacy(a) || keycrypt.StoredMode(a) != keycrypt.ModeStatic {
		t.Fatalf("stored mode of %q = %q", a, keycrypt.StoredMode(a))
	}
}

func TestStaticAADBindsAppName(t *testing.T) {
	s := newStatic(t, 1)
	ctx := context.Background()
	ct, _ := s.Encrypt(ctx, "ci", []byte(pemText))
	if _, err := s.Decrypt(ctx, "other", ct); err == nil {
		t.Fatal("ciphertext for app ci decrypted as app other")
	}
}

func TestStaticRejectsWrongKeyAndTampering(t *testing.T) {
	ctx := context.Background()
	ct, _ := newStatic(t, 1).Encrypt(ctx, "ci", []byte(pemText))
	if _, err := newStatic(t, 2).Decrypt(ctx, "ci", ct); err == nil {
		t.Fatal("decrypted with the wrong key")
	}
	raw, _ := base64.StdEncoding.DecodeString(strings.TrimPrefix(ct, "enc:v1:static:"))
	raw[len(raw)-1] ^= 1
	tampered := "enc:v1:static:" + base64.StdEncoding.EncodeToString(raw)
	for _, bad := range []string{tampered, "enc:v1:static:!!", "enc:v1:static:" + base64.StdEncoding.EncodeToString([]byte("short"))} {
		if _, err := newStatic(t, 1).Decrypt(ctx, "ci", bad); err == nil {
			t.Fatalf("accepted %q", bad)
		}
	}
	if _, err := keycrypt.NewStatic(make([]byte, 16)); err == nil {
		t.Fatal("accepted a 16-byte key")
	}
}

func TestModeMismatchAndLegacy(t *testing.T) {
	ctx := context.Background()
	s := newStatic(t, 1)
	ct, _ := s.Encrypt(ctx, "ci", []byte(pemText))
	if _, err := (keycrypt.None{}).Decrypt(ctx, "ci", ct); err == nil || !strings.Contains(err.Error(), `"static"`) {
		t.Fatalf("none decrypting static ciphertext: %v", err)
	}
	if _, err := s.Decrypt(ctx, "ci", "enc:v1:transit:vault:v1:abc"); err == nil || !strings.Contains(err.Error(), `"transit"`) {
		t.Fatalf("static decrypting transit ciphertext: %v", err)
	}
	if pt, err := (keycrypt.None{}).Decrypt(ctx, "ci", pemText); err != nil || string(pt) != pemText {
		t.Fatalf("none legacy passthrough = %q, %v", pt, err)
	}
	if pt, err := s.Decrypt(ctx, "ci", pemText); err == nil || strings.Contains(err.Error(), "MIIEsecret") {
		t.Fatalf("static accepted unprefixed value: %q, %v", pt, err)
	}
	if stored, _ := (keycrypt.None{}).Encrypt(ctx, "ci", []byte(pemText)); stored != pemText {
		t.Fatalf("none stored %q", stored)
	}
}

func env(m map[string]string) func(string, string) string {
	return func(k, def string) string {
		if v, ok := m[k]; ok {
			return v
		}
		return def
	}
}

func TestConfigFromEnv(t *testing.T) {
	key := base64.StdEncoding.EncodeToString(staticKey(7))
	transit := map[string]string{"GITHUB_APP_KEY_ENCRYPTION": "transit", "VAULT_ADDR": "https://vault:8200",
		"VAULT_K8S_ROLE": "artifactapi", "VAULT_TRANSIT_KEY": "github-apps"}
	with := func(base map[string]string, k, v string) map[string]string {
		m := map[string]string{}
		for a, b := range base {
			m[a] = b
		}
		if v == "<unset>" {
			delete(m, k)
		} else {
			m[k] = v
		}
		return m
	}
	for _, tc := range []struct {
		name    string
		env     map[string]string
		wantErr string
	}{
		{"default none", nil, ""},
		{"explicit none", map[string]string{"GITHUB_APP_KEY_ENCRYPTION": "none"}, ""},
		{"unknown mode", map[string]string{"GITHUB_APP_KEY_ENCRYPTION": "aes"}, "invalid GITHUB_APP_KEY_ENCRYPTION"},
		{"decrypt-only key under none", map[string]string{"GITHUB_APP_KEY_ENCRYPTION_KEY": key}, ""},
		{"decrypt-only short key", map[string]string{"GITHUB_APP_KEY_ENCRYPTION": "transit", "VAULT_ADDR": "https://vault:8200",
			"VAULT_K8S_ROLE": "r", "VAULT_TRANSIT_KEY": "k", "GITHUB_APP_KEY_ENCRYPTION_KEY": base64.StdEncoding.EncodeToString(make([]byte, 8))}, "exactly 32 bytes"},
		{"transit plus static key", with(transit, "GITHUB_APP_KEY_ENCRYPTION_KEY", key), ""},
		{"static plus decrypt-only transit", with(with(transit, "GITHUB_APP_KEY_ENCRYPTION", "static"), "GITHUB_APP_KEY_ENCRYPTION_KEY", key), ""},
		{"decrypt-only transit missing role", with(with(with(transit, "GITHUB_APP_KEY_ENCRYPTION", "static"),
			"GITHUB_APP_KEY_ENCRYPTION_KEY", key), "VAULT_K8S_ROLE", "<unset>"), "VAULT_K8S_ROLE"},
		{"transit http", with(transit, "VAULT_ADDR", "http://vault:8200"), "VAULT_ALLOW_INSECURE_HTTP"},
		{"transit http allowed", with(with(transit, "VAULT_ADDR", "http://vault:8200"), "VAULT_ALLOW_INSECURE_HTTP", "true"), ""},
		{"insecure flag invalid", with(transit, "VAULT_ALLOW_INSECURE_HTTP", "maybe"), "VAULT_ALLOW_INSECURE_HTTP"},
		{"static", map[string]string{"GITHUB_APP_KEY_ENCRYPTION": "static", "GITHUB_APP_KEY_ENCRYPTION_KEY": key}, ""},
		{"static missing key", map[string]string{"GITHUB_APP_KEY_ENCRYPTION": "static"}, "exactly 32 bytes"},
		{"static short key", map[string]string{"GITHUB_APP_KEY_ENCRYPTION": "static",
			"GITHUB_APP_KEY_ENCRYPTION_KEY": base64.StdEncoding.EncodeToString(make([]byte, 31))}, "exactly 32 bytes"},
		{"static bad base64", map[string]string{"GITHUB_APP_KEY_ENCRYPTION": "static", "GITHUB_APP_KEY_ENCRYPTION_KEY": "%%%"}, "base64"},
		{"transit", transit, ""},
		{"transit no role", with(transit, "VAULT_K8S_ROLE", "<unset>"), "VAULT_K8S_ROLE"},
		{"transit no key", with(transit, "VAULT_TRANSIT_KEY", "<unset>"), "VAULT_TRANSIT_KEY"},
		{"transit no addr", with(transit, "VAULT_ADDR", "<unset>"), "VAULT_ADDR"},
		{"transit bad addr", with(transit, "VAULT_ADDR", "vault:8200"), "invalid VAULT_ADDR"},
		{"transit empty mount", with(transit, "VAULT_TRANSIT_MOUNT", ""), "VAULT_TRANSIT_MOUNT"},
		{"transit key with slash", with(transit, "VAULT_TRANSIT_KEY", "a/b"), "invalid VAULT_TRANSIT_KEY"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := keycrypt.ConfigFromEnv(env(tc.env))
			if tc.wantErr == "" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v; want %q", err, tc.wantErr)
			}
			if strings.Contains(err.Error(), key) {
				t.Fatal("error leaks the static key")
			}
			_ = cfg
		})
	}
	cfg, _ := keycrypt.ConfigFromEnv(env(transit))
	if v := cfg.Vault; v.K8sMount != "kubernetes" || v.TransitMount != "transit" ||
		v.TokenPath != "/var/run/secrets/kubernetes.io/serviceaccount/token" {
		t.Fatalf("transit defaults: %+v", v)
	}
	if _, err := keycrypt.New(context.Background(), keycrypt.Config{Mode: "static", StaticKey: make([]byte, 8)}); err == nil {
		t.Fatal("New accepted an invalid config")
	}
}

func TestKeyringDecryptsEveryConfiguredMode(t *testing.T) {
	ctx := context.Background()
	oldKey, newKey := newStatic(t, 1), newStatic(t, 2)
	ct, _ := oldKey.Encrypt(ctx, "ci", []byte(pemText))

	k := keycrypt.NewKeyring(newKey, keycrypt.None{})
	if k.Mode() != keycrypt.ModeStatic {
		t.Fatalf("mode = %s", k.Mode())
	}
	if _, err := k.Decrypt(ctx, "ci", ct); err == nil {
		t.Fatal("keyring preferred the read-only None over the static write cipher")
	}
	written, _ := k.Encrypt(ctx, "ci", []byte(pemText))
	if pt, err := newKey.Decrypt(ctx, "ci", written); err != nil || string(pt) != pemText {
		t.Fatalf("keyring did not write with the selected cipher: %v", err)
	}

	strict := keycrypt.NewKeyring(newKey)
	if _, err := strict.Decrypt(ctx, "ci", pemText); err == nil || !strings.Contains(err.Error(), "not encrypted") {
		t.Fatalf("legacy under static keyring: %v", err)
	}
	if _, err := strict.Decrypt(ctx, "ci", "enc:v1:transit:vault:v1:x"); err == nil || !strings.Contains(err.Error(), "not configured") {
		t.Fatalf("unconfigured transit: %v", err)
	}
	if strict.CanDecrypt("enc:v1:transit:vault:v1:x") || !strict.CanDecrypt(written) {
		t.Fatal("CanDecrypt wrong")
	}
}

func TestNewBuildsDecryptOnlyCiphers(t *testing.T) {
	ctx := context.Background()
	old, _ := newStatic(t, 3).Encrypt(ctx, "ci", []byte(pemText))
	c, err := keycrypt.New(ctx, keycrypt.Config{Mode: keycrypt.ModeStatic, StaticKey: staticKey(3)})
	if err != nil {
		t.Fatal(err)
	}
	if pt, err := c.Decrypt(ctx, "ci", old); err != nil || string(pt) != pemText {
		t.Fatalf("static via New: %v", err)
	}
	n, err := keycrypt.New(ctx, keycrypt.Config{Mode: keycrypt.ModeNone, StaticKey: staticKey(3)})
	if err != nil || n.Mode() != keycrypt.ModeNone {
		t.Fatalf("none with decrypt-only key: %v", err)
	}
	if _, err := n.Decrypt(ctx, "ci", old); err == nil {
		t.Fatal("none mode decrypted ciphertext")
	}
}
