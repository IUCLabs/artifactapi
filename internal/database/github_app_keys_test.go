package database

import (
	"bytes"
	"context"
	"fmt"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/IUCLabs/artifactapi/internal/keycrypt"
	"github.com/IUCLabs/artifactapi/internal/keycrypt/keycrypttest"
	"github.com/IUCLabs/artifactapi/pkg/models"
)

var keyDBSeq atomic.Int64

// isolatedDB returns a migrated database of its own, since LoadGitHubApps
// rewrites every row in github_apps.
func isolatedDB(t *testing.T) *DB {
	t.Helper()
	requireDB(t)
	name := fmt.Sprintf("keys_%d", keyDBSeq.Add(1))
	if _, err := testDB.Pool.Exec(ctx(), "CREATE DATABASE "+name); err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(testDSN)
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + name
	db, err := New(u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		db.Close()
		_, _ = testDB.Pool.Exec(context.Background(), "DROP DATABASE "+name)
	})
	return db
}

func withCipher(db *DB, c keycrypt.Cipher) *DB { return &DB{Pool: db.Pool, keys: c} }

func staticCipher(t *testing.T, b byte) keycrypt.Cipher {
	t.Helper()
	c, err := keycrypt.NewStatic(bytes.Repeat([]byte{b}, 32))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func rawKey(t *testing.T, db *DB, name string) string {
	t.Helper()
	var s string
	if err := db.Pool.QueryRow(ctx(), `SELECT private_key FROM github_apps WHERE name = $1`, name).Scan(&s); err != nil {
		t.Fatal(err)
	}
	return s
}

func pemBody(keyPEM string) string {
	lines := strings.Split(keyPEM, "\n")
	return lines[1]
}

func assertSealed(t *testing.T, db *DB, name, keyPEM, prefix string) string {
	t.Helper()
	raw := rawKey(t, db, name)
	if !strings.HasPrefix(raw, prefix) || strings.Contains(raw, "PRIVATE KEY") || strings.Contains(raw, pemBody(keyPEM)) {
		t.Fatalf("%s column = %.60q...; want %s ciphertext without PEM material", name, raw, prefix)
	}
	return raw
}

func TestGitHubAppKeysEncryptedAtRest(t *testing.T) {
	v := keycrypttest.NewVault(t)
	transit, err := keycrypt.NewTransit(v.Config())
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		cipher keycrypt.Cipher
		prefix string
	}{
		{staticCipher(t, 1), "enc:v1:static:"},
		{transit, "enc:v1:transit:vault:v1:"},
	} {
		t.Run(tc.cipher.Mode(), func(t *testing.T) {
			db := withCipher(isolatedDB(t), tc.cipher)
			keyPEM := testAppKeyPEM(t)
			put := models.GitHubApp{Name: "put", AppID: 1, InstallationID: 2, PrivateKey: keyPEM}
			if err := db.PutGitHubApp(ctx(), &put); err != nil {
				t.Fatal(err)
			}
			created := models.GitHubApp{Name: "created", AppID: 3, PrivateKey: keyPEM}
			if err := db.CreateGitHubApp(ctx(), &created); err != nil {
				t.Fatal(err)
			}
			first := assertSealed(t, db, "put", keyPEM, tc.prefix)
			assertSealed(t, db, "created", keyPEM, tc.prefix)

			again := put
			if err := db.PutGitHubApp(ctx(), &again); err != nil {
				t.Fatal(err)
			}
			if !again.UpdatedAt.Equal(put.UpdatedAt) {
				t.Fatalf("identical encrypted put moved updated_at %v -> %v", put.UpdatedAt, again.UpdatedAt)
			}
			assertSealed(t, db, "put", keyPEM, tc.prefix)
			if tc.cipher.Mode() == keycrypt.ModeStatic && rawKey(t, db, "put") == first {
				t.Fatal("static re-put reused the nonce")
			}

			got, err := db.GetGitHubApp(ctx(), "put")
			if err != nil || got.PrivateKey != keyPEM {
				t.Fatalf("get decrypted = %v; key match %v", err, got != nil && got.PrivateKey == keyPEM)
			}
			apps, err := db.LoadGitHubApps(ctx())
			if err != nil || len(apps) != 2 || apps[0].PrivateKey != keyPEM || apps[1].PrivateKey != keyPEM {
				t.Fatalf("load: %d apps, %v", len(apps), err)
			}
		})
	}
}

func TestGitHubAppKeysSealsLegacyRows(t *testing.T) {
	plain := isolatedDB(t)
	keys := map[string]string{"a": testAppKeyPEM(t), "b": testAppKeyPEM(t)}
	updated := map[string]models.GitHubApp{}
	for name, k := range keys {
		a := models.GitHubApp{Name: name, AppID: 1, InstallationID: 2, PrivateKey: k}
		if err := plain.PutGitHubApp(ctx(), &a); err != nil {
			t.Fatal(err)
		}
		updated[name] = a
		if rawKey(t, plain, name) != k {
			t.Fatal("none mode did not store plaintext")
		}
	}

	db := withCipher(plain, staticCipher(t, 1))
	apps, err := db.LoadGitHubApps(ctx())
	if err != nil || len(apps) != 2 {
		t.Fatalf("load: %d apps, %v", len(apps), err)
	}
	sealed := map[string]string{}
	for _, a := range apps {
		if a.PrivateKey != keys[a.Name] || !a.UpdatedAt.Equal(updated[a.Name].UpdatedAt) {
			t.Fatalf("%s: key match %v, updated_at %v -> %v", a.Name, a.PrivateKey == keys[a.Name], updated[a.Name].UpdatedAt, a.UpdatedAt)
		}
		sealed[a.Name] = assertSealed(t, db, a.Name, keys[a.Name], "enc:v1:static:")
	}
	if _, err := db.LoadGitHubApps(ctx()); err != nil {
		t.Fatal(err)
	}
	for name, s := range sealed {
		if rawKey(t, db, name) != s {
			t.Fatalf("%s re-sealed on second load", name)
		}
	}
	if _, err := plain.LoadGitHubApps(ctx()); err == nil || !strings.Contains(err.Error(), `"a"`) {
		t.Fatalf("none mode over sealed rows: %v; want error naming the app", err)
	}
}

func TestGitHubAppKeysWrongKeyFailsLoad(t *testing.T) {
	base := isolatedDB(t)
	db := withCipher(base, staticCipher(t, 1))
	a := models.GitHubApp{Name: "ci", AppID: 1, InstallationID: 2, PrivateKey: testAppKeyPEM(t)}
	if err := db.PutGitHubApp(ctx(), &a); err != nil {
		t.Fatal(err)
	}
	_, err := withCipher(base, staticCipher(t, 2)).LoadGitHubApps(ctx())
	if err == nil || !strings.Contains(err.Error(), `github app "ci"`) || strings.Contains(err.Error(), "PRIVATE KEY") {
		t.Fatalf("wrong key: %v; want an error naming the app", err)
	}
}

func TestGitHubAppKeysBoundToRow(t *testing.T) {
	db := withCipher(isolatedDB(t), staticCipher(t, 1))
	for _, name := range []string{"a", "b"} {
		a := models.GitHubApp{Name: name, AppID: 1, InstallationID: 2, PrivateKey: testAppKeyPEM(t)}
		if err := db.PutGitHubApp(ctx(), &a); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Pool.Exec(ctx(), `UPDATE github_apps SET private_key = (SELECT private_key FROM github_apps WHERE name = 'a') WHERE name = 'b'`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.GetGitHubApp(ctx(), "b"); err == nil {
		t.Fatal("app b decrypted app a's ciphertext")
	}
	if _, err := db.LoadGitHubApps(ctx()); err == nil || !strings.Contains(err.Error(), `"b"`) {
		t.Fatalf("load with moved ciphertext: %v", err)
	}
}
