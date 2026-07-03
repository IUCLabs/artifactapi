// Package tfsign loads a GPG signing key and produces the detached signatures
// the Terraform provider registry protocol requires over SHA256SUMS files.
package tfsign

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"strings"

	"golang.org/x/crypto/openpgp"
	"golang.org/x/crypto/openpgp/armor"
)

// KeyStore persists a generated signing key. *database.DB satisfies it.
type KeyStore interface {
	GetSigningKey(ctx context.Context, purpose string) (armor, keyID string, found bool, err error)
	InsertSigningKeyIfAbsent(ctx context.Context, purpose, armor, keyID string) error
}

// LoadOrCreate returns a signer for purpose, generating and persisting a new key
// the first time it is needed. It is safe across replicas: a lost insert race
// just re-reads whichever key won.
func LoadOrCreate(ctx context.Context, store KeyStore, purpose string) (*Signer, error) {
	armored, _, found, err := store.GetSigningKey(ctx, purpose)
	if err != nil {
		return nil, err
	}
	if !found {
		newArmor, keyID, err := Generate()
		if err != nil {
			return nil, err
		}
		if err := store.InsertSigningKeyIfAbsent(ctx, purpose, newArmor, keyID); err != nil {
			return nil, err
		}
		if armored, _, _, err = store.GetSigningKey(ctx, purpose); err != nil {
			return nil, err
		}
	}
	return LoadArmored(armored, "")
}

// Signer holds a decrypted GPG entity and exposes what the registry download
// response needs: a detached signature, the armored public key, and the key ID.
type Signer struct {
	entity      *openpgp.Entity
	publicASCII string
	keyID       string
}

// Load reads an armored private key from path, decrypting it with passphrase if
// the key is protected. A blank path returns (nil, nil): a nil *Signer means the
// caller should fall back to another source (e.g. a DB-stored key).
func Load(path, passphrase string) (*Signer, error) {
	if path == "" {
		return nil, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("open signing key: %w", err)
	}
	return fromArmor(string(data), passphrase, path)
}

// LoadArmored builds a signer from an in-memory armored private key, e.g. one
// read from the database. A blank key returns (nil, nil).
func LoadArmored(armored, passphrase string) (*Signer, error) {
	if armored == "" {
		return nil, nil
	}
	return fromArmor(armored, passphrase, "stored key")
}

// Generate creates a fresh signing keypair and returns the armored private key
// (to persist) and its uppercase key id.
func Generate() (armoredPrivateKey, keyID string, err error) {
	entity, err := openpgp.NewEntity("artifactapi terraform registry", "provider signing", "artifactapi@localhost", nil)
	if err != nil {
		return "", "", err
	}
	var buf bytes.Buffer
	w, err := armor.Encode(&buf, openpgp.PrivateKeyType, nil)
	if err != nil {
		return "", "", err
	}
	if err := entity.SerializePrivate(w, nil); err != nil {
		return "", "", err
	}
	if err := w.Close(); err != nil {
		return "", "", err
	}
	return buf.String(), strings.ToUpper(entity.PrimaryKey.KeyIdString()), nil
}

func fromArmor(armored, passphrase, src string) (*Signer, error) {
	keyring, err := openpgp.ReadArmoredKeyRing(strings.NewReader(armored))
	if err != nil {
		return nil, fmt.Errorf("read signing key: %w", err)
	}
	if len(keyring) == 0 {
		return nil, fmt.Errorf("signing key (%s) contains no entities", src)
	}
	entity := keyring[0]

	if entity.PrivateKey == nil {
		return nil, fmt.Errorf("signing key (%s) has no private key material", src)
	}
	if entity.PrivateKey.Encrypted {
		if err := decrypt(entity, passphrase); err != nil {
			return nil, err
		}
	}

	pub, err := armorPublicKey(entity)
	if err != nil {
		return nil, err
	}

	return &Signer{
		entity:      entity,
		publicASCII: pub,
		keyID:       entity.PrimaryKey.KeyIdString(),
	}, nil
}

// decrypt unlocks the entity's private key and all subkeys with the passphrase.
func decrypt(entity *openpgp.Entity, passphrase string) error {
	pw := []byte(passphrase)
	if err := entity.PrivateKey.Decrypt(pw); err != nil {
		return fmt.Errorf("decrypt signing key: %w", err)
	}
	for _, sub := range entity.Subkeys {
		if sub.PrivateKey != nil && sub.PrivateKey.Encrypted {
			_ = sub.PrivateKey.Decrypt(pw)
		}
	}
	return nil
}

func armorPublicKey(entity *openpgp.Entity) (string, error) {
	var buf bytes.Buffer
	w, err := armor.Encode(&buf, openpgp.PublicKeyType, nil)
	if err != nil {
		return "", err
	}
	if err := entity.Serialize(w); err != nil {
		return "", err
	}
	if err := w.Close(); err != nil {
		return "", err
	}
	return buf.String(), nil
}

// Sign returns a binary detached signature over message, matching the
// SHA256SUMS.sig format Terraform verifies.
func (s *Signer) Sign(message []byte) ([]byte, error) {
	var buf bytes.Buffer
	if err := openpgp.DetachSign(&buf, s.entity, bytes.NewReader(message), nil); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// PublicKeyArmor returns the ASCII-armored public key for the registry's
// signing_keys response.
func (s *Signer) PublicKeyArmor() string { return s.publicASCII }

// KeyID returns the 16-hex-char uppercase key ID Terraform matches against the
// signature's issuer.
func (s *Signer) KeyID() string { return strings.ToUpper(s.keyID) }
