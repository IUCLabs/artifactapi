package tfsign

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"regexp"
	"testing"

	"golang.org/x/crypto/openpgp"
	"golang.org/x/crypto/openpgp/armor"
)

// armoredPrivateKey generates a throwaway armored private key for tests.
func armoredPrivateKey(t *testing.T) string {
	t.Helper()
	e, err := openpgp.NewEntity("artifactapi test", "tf registry", "tf@example.com", nil)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	w, err := armor.Encode(&buf, openpgp.PrivateKeyType, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.SerializePrivate(w, nil); err != nil {
		t.Fatal(err)
	}
	w.Close()
	return buf.String()
}

func writeKey(t *testing.T, contents string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "private-key.asc")
	if err := os.WriteFile(p, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadSignAndVerify(t *testing.T) {
	path := writeKey(t, armoredPrivateKey(t))
	s, err := Load(path, "")
	if err != nil {
		t.Fatal(err)
	}
	if s == nil {
		t.Fatal("expected a signer")
	}

	if !regexp.MustCompile(`^[0-9A-F]{16}$`).MatchString(s.KeyID()) {
		t.Errorf("key id %q is not 16 uppercase hex chars", s.KeyID())
	}

	msg := []byte("deadbeef  terraform-provider-x_1.0.0_linux_amd64.zip\n")
	sig, err := s.Sign(msg)
	if err != nil {
		t.Fatal(err)
	}

	// The advertised public key must verify the signature over the same bytes.
	keyring, err := openpgp.ReadArmoredKeyRing(bytes.NewReader([]byte(s.PublicKeyArmor())))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := openpgp.CheckDetachedSignature(keyring, bytes.NewReader(msg), bytes.NewReader(sig)); err != nil {
		t.Errorf("signature did not verify: %v", err)
	}
}

func TestGenerateAndLoadArmored(t *testing.T) {
	priv, keyID, err := Generate()
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`^[0-9A-F]{16}$`).MatchString(keyID) {
		t.Errorf("generated key id %q malformed", keyID)
	}

	s, err := LoadArmored(priv, "")
	if err != nil {
		t.Fatal(err)
	}
	if s.KeyID() != keyID {
		t.Errorf("loaded key id %q != generated %q", s.KeyID(), keyID)
	}

	msg := []byte("abc  terraform-provider-x_1.0.0_linux_amd64.zip\n")
	sig, err := s.Sign(msg)
	if err != nil {
		t.Fatal(err)
	}
	keyring, _ := openpgp.ReadArmoredKeyRing(bytes.NewReader([]byte(s.PublicKeyArmor())))
	if _, err := openpgp.CheckDetachedSignature(keyring, bytes.NewReader(msg), bytes.NewReader(sig)); err != nil {
		t.Errorf("signature did not verify: %v", err)
	}
}

// memStore is an in-memory KeyStore that records how many keys it accepted.
type memStore struct {
	armor, keyID string
	found        bool
	inserts      int
}

func (m *memStore) GetSigningKey(_ context.Context, _ string) (string, string, bool, error) {
	return m.armor, m.keyID, m.found, nil
}

func (m *memStore) InsertSigningKeyIfAbsent(_ context.Context, _, armor, keyID string) error {
	if !m.found { // ON CONFLICT DO NOTHING
		m.armor, m.keyID, m.found = armor, keyID, true
		m.inserts++
	}
	return nil
}

func TestLoadOrCreateGeneratesOnceThenReuses(t *testing.T) {
	store := &memStore{}

	first, err := LoadOrCreate(context.Background(), store, "terraform-provider")
	if err != nil || first == nil {
		t.Fatalf("first LoadOrCreate: signer=%v err=%v", first, err)
	}

	second, err := LoadOrCreate(context.Background(), store, "terraform-provider")
	if err != nil || second == nil {
		t.Fatalf("second LoadOrCreate: signer=%v err=%v", second, err)
	}

	if store.inserts != 1 {
		t.Errorf("expected exactly one key generated, got %d", store.inserts)
	}
	if first.KeyID() != second.KeyID() {
		t.Errorf("key id changed between loads: %q vs %q", first.KeyID(), second.KeyID())
	}
}

func TestLoadEmptyPathDisabled(t *testing.T) {
	s, err := Load("", "")
	if err != nil {
		t.Fatal(err)
	}
	if s != nil {
		t.Error("empty path should yield a nil (disabled) signer")
	}
}

func TestLoadMissingFile(t *testing.T) {
	if _, err := Load(filepath.Join(t.TempDir(), "nope.asc"), ""); err == nil {
		t.Error("expected an error for a missing key file")
	}
}
