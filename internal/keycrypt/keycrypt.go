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
	// Legacy unprefixed values are returned as-is.
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
	return fmt.Errorf("private key is encrypted with mode %q but GITHUB_APP_KEY_ENCRYPTION=%s", StoredMode(stored), mode)
}

// New builds the Cipher for cfg; a transit cipher logs in and probes Vault.
func New(ctx context.Context, cfg Config) (Cipher, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	switch cfg.Mode {
	case ModeStatic:
		return NewStatic(cfg.StaticKey)
	case ModeTransit:
		t, err := NewTransit(cfg.Vault)
		if err != nil {
			return nil, err
		}
		if err := t.Check(ctx); err != nil {
			return nil, err
		}
		return t, nil
	}
	return None{}, nil
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
	if IsLegacy(stored) {
		return []byte(stored), nil
	}
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
