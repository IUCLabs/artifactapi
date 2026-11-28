// Package keycrypt encrypts GitHub App private keys at rest.
package keycrypt

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
)

// Modes for GITHUB_APP_KEY_ENCRYPTION.
const (
	ModeNone    = "none"
	ModeStatic  = "static"
	ModeTransit = "transit"
)

const (
	prefix        = "enc:v1:"
	staticPrefix  = prefix + ModeStatic + ":"
	transitPrefix = prefix + ModeTransit + ":"
)

// Cipher seals and opens private keys. name is the owning app's name.
type Cipher interface {
	Mode() string
	Encrypt(ctx context.Context, name string, plaintext []byte) (string, error)
	// Decrypt returns the plaintext; the caller should Zero it after use.
	// Only None accepts legacy unprefixed values.
	Decrypt(ctx context.Context, name, stored string) ([]byte, error)
}

// IsLegacy reports whether stored is an unencrypted (pre-encryption) value.
func IsLegacy(stored string) bool { return !strings.HasPrefix(stored, prefix) }

// StoredMode returns the mode that produced stored ("none" for legacy values).
func StoredMode(stored string) string {
	if IsLegacy(stored) {
		return ModeNone
	}
	mode, _, _ := strings.Cut(strings.TrimPrefix(stored, prefix), ":")
	return mode
}

// Zero overwrites b.
func Zero(b []byte) { clear(b) }

func mismatch(stored, mode string) error {
	if IsLegacy(stored) {
		return fmt.Errorf("private key is not encrypted but GITHUB_APP_KEY_ENCRYPTION=%s", mode)
	}
	return fmt.Errorf("private key is encrypted with mode %q but GITHUB_APP_KEY_ENCRYPTION=%s", StoredMode(stored), mode)
}

// New builds the Cipher for cfg: it encrypts with the selected mode and
// decrypts with every configured cipher. A transit write cipher logs in and
// probes Vault. Mode none ignores other ciphers.
func New(ctx context.Context, cfg Config) (Cipher, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if cfg.Mode == ModeNone || cfg.Mode == "" {
		return None{}, nil
	}
	var write Cipher
	var others []Cipher
	if cfg.HasStatic() {
		s, err := NewStatic(cfg.StaticKey)
		if err != nil {
			return nil, err
		}
		others = append(others, s)
	}
	if cfg.HasTransit() {
		t, err := NewTransit(cfg.Vault)
		if err != nil {
			return nil, err
		}
		if cfg.Mode == ModeTransit {
			if err := t.Check(ctx); err != nil {
				return nil, err
			}
		}
		others = append(others, t)
	}
	for i, c := range others {
		if c.Mode() == cfg.Mode {
			write = c
			others = append(others[:i], others[i+1:]...)
			break
		}
	}
	return NewKeyring(write, others...), nil
}

// Keyring encrypts with one cipher and decrypts with whichever configured
// cipher matches a value's mode prefix.
type Keyring struct {
	write Cipher
	byMod map[string]Cipher
}

// NewKeyring returns a Keyring writing with write and also reading others.
func NewKeyring(write Cipher, others ...Cipher) *Keyring {
	k := &Keyring{write: write, byMod: map[string]Cipher{write.Mode(): write}}
	for _, c := range others {
		if _, ok := k.byMod[c.Mode()]; !ok {
			k.byMod[c.Mode()] = c
		}
	}
	return k
}

func (k *Keyring) Mode() string { return k.write.Mode() }

// CanDecrypt reports whether a cipher for stored's mode is configured.
func (k *Keyring) CanDecrypt(stored string) bool {
	_, ok := k.byMod[StoredMode(stored)]
	return ok
}

func (k *Keyring) Encrypt(ctx context.Context, name string, plaintext []byte) (string, error) {
	return k.write.Encrypt(ctx, name, plaintext)
}

func (k *Keyring) Decrypt(ctx context.Context, name, stored string) ([]byte, error) {
	c, ok := k.byMod[StoredMode(stored)]
	if !ok {
		if IsLegacy(stored) {
			return nil, fmt.Errorf("private key is not encrypted but GITHUB_APP_KEY_ENCRYPTION=%s", k.Mode())
		}
		return nil, fmt.Errorf("private key is encrypted with mode %q, which is not configured", StoredMode(stored))
	}
	return c.Decrypt(ctx, name, stored)
}

// None stores keys as plaintext and refuses encrypted values.
type None struct{}

func (None) Mode() string { return ModeNone }

func (None) Encrypt(_ context.Context, _ string, plaintext []byte) (string, error) {
	return string(plaintext), nil
}

func (None) Decrypt(_ context.Context, _, stored string) ([]byte, error) {
	if !IsLegacy(stored) {
		return nil, mismatch(stored, ModeNone)
	}
	return []byte(stored), nil
}

// Static is AES-256-GCM with a random nonce per value and the app name as AAD.
type Static struct {
	aead cipher.AEAD
}

// NewStatic builds a Static cipher from a 32-byte key.
func NewStatic(key []byte) (*Static, error) {
	if len(key) != 32 {
		return nil, errors.New("static key must be exactly 32 bytes")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Static{aead: aead}, nil
}

func (s *Static) Mode() string { return ModeStatic }

func (s *Static) Encrypt(_ context.Context, name string, plaintext []byte) (string, error) {
	nonce := make([]byte, s.aead.NonceSize(), s.aead.NonceSize()+len(plaintext)+s.aead.Overhead())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	sealed := s.aead.Seal(nonce, nonce, plaintext, []byte(name))
	return staticPrefix + base64.StdEncoding.EncodeToString(sealed), nil
}

func (s *Static) Decrypt(_ context.Context, name, stored string) ([]byte, error) {
	if !strings.HasPrefix(stored, staticPrefix) {
		return nil, mismatch(stored, ModeStatic)
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(stored, staticPrefix))
	if err != nil || len(raw) < s.aead.NonceSize()+s.aead.Overhead() {
		return nil, errors.New("malformed static ciphertext")
	}
	nonce, ct := raw[:s.aead.NonceSize()], raw[s.aead.NonceSize():]
	pt, err := s.aead.Open(nil, nonce, ct, []byte(name))
	if err != nil {
		return nil, errors.New("static decryption failed (wrong key, or ciphertext belongs to another app)")
	}
	return pt, nil
}
