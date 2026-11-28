package database

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/IUCLabs/golib/pg"

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

			installed, err := db.SetGitHubAppInstallation(ctx(), "created", 3, 9)
			if err != nil || installed.PrivateKey != keyPEM {
				t.Fatalf("set installation: %v; key decrypted %v", err, installed != nil && installed.PrivateKey == keyPEM)
			}
			assertSealed(t, db, "created", keyPEM, tc.prefix)

			for _, name := range []string{"put", "created"} {
				got, err := db.GetGitHubApp(ctx(), name)
				if err != nil || got.PrivateKey != keyPEM {
					t.Fatalf("get %s decrypted = %v; key match %v", name, err, got != nil && got.PrivateKey == keyPEM)
				}
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
	if _, err := plain.LoadGitHubApps(ctx()); err == nil || !strings.Contains(err.Error(), `"a"`) || !strings.Contains(err.Error(), "rollback") {
		t.Fatalf("none mode over sealed rows: %v; want error naming the app", err)
	}
	for name, s := range sealed {
		if rawKey(t, db, name) != s {
			t.Fatalf("none mode rewrote %s", name)
		}
	}
}

func transitCipher(t *testing.T) keycrypt.Cipher {
	t.Helper()
	c, err := keycrypt.NewTransit(keycrypttest.NewVault(t).Config())
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestGitHubAppKeysReKey(t *testing.T) {
	static, transit := staticCipher(t, 1), transitCipher(t)
	for _, tc := range []struct {
		name     string
		from, to keycrypt.Cipher
		prefix   string
	}{
		{"static to transit", static, transit, "enc:v1:transit:"},
		{"transit to static", transit, static, "enc:v1:static:"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base := isolatedDB(t)
			keyPEM := testAppKeyPEM(t)
			a := models.GitHubApp{Name: "ci", AppID: 1, InstallationID: 2, PrivateKey: keyPEM}
			if err := withCipher(base, tc.from).PutGitHubApp(ctx(), &a); err != nil {
				t.Fatal(err)
			}
			if _, err := withCipher(base, keycrypt.NewKeyring(tc.to)).LoadGitHubApps(ctx()); err == nil {
				t.Fatal("loaded ciphertext of an unconfigured cipher")
			}
			db := withCipher(base, keycrypt.NewKeyring(tc.to, tc.from))
			apps, err := db.LoadGitHubApps(ctx())
			if err != nil || len(apps) != 1 || apps[0].PrivateKey != keyPEM || !apps[0].UpdatedAt.Equal(a.UpdatedAt) {
				t.Fatalf("re-key load: %d apps, %v", len(apps), err)
			}
			assertSealed(t, db, "ci", keyPEM, tc.prefix)
			got, err := withCipher(base, tc.to).GetGitHubApp(ctx(), "ci")
			if err != nil || got.PrivateKey != keyPEM {
				t.Fatalf("new cipher alone after re-key: %v", err)
			}
		})
	}
}

func TestGitHubAppKeysRejectInjectedPlaintext(t *testing.T) {
	db := withCipher(isolatedDB(t), staticCipher(t, 1))
	keyPEM := testAppKeyPEM(t)
	a := models.GitHubApp{Name: "ci", AppID: 1, InstallationID: 2, PrivateKey: testAppKeyPEM(t)}
	if err := db.PutGitHubApp(ctx(), &a); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Pool.Exec(ctx(), `UPDATE github_apps SET private_key = $1 WHERE name = 'ci'`, keyPEM); err != nil {
		t.Fatal(err)
	}
	got, err := db.GetGitHubApp(ctx(), "ci")
	if err == nil || strings.Contains(err.Error(), pemBody(keyPEM)) {
		t.Fatalf("injected plaintext read back: %v (key returned %v)", err, got != nil)
	}
	if _, err := db.SetGitHubAppInstallation(ctx(), "ci", 1, 3); err == nil {
		t.Fatal("set installation returned an injected plaintext key")
	}
}

type countingCipher struct {
	keycrypt.Cipher
	failOn   string
	encrypts atomic.Int64
}

func (c *countingCipher) Encrypt(ctx context.Context, name string, pt []byte) (string, error) {
	c.encrypts.Add(1)
	if name == c.failOn {
		return "", errors.New("injected encrypt failure")
	}
	return c.Cipher.Encrypt(ctx, name, pt)
}

func seedPlaintext(t *testing.T, db *DB, names ...string) map[string]string {
	t.Helper()
	keys := map[string]string{}
	for _, name := range names {
		keys[name] = testAppKeyPEM(t)
		a := models.GitHubApp{Name: name, AppID: 1, InstallationID: 2, PrivateKey: keys[name]}
		if err := db.PutGitHubApp(ctx(), &a); err != nil {
			t.Fatal(err)
		}
	}
	return keys
}

func TestGitHubAppKeysPartialSealFailure(t *testing.T) {
	base := isolatedDB(t)
	keys := seedPlaintext(t, base, "a", "b", "c")
	static := staticCipher(t, 1)
	_, err := withCipher(base, &countingCipher{Cipher: static, failOn: "b"}).LoadGitHubApps(ctx())
	if err == nil || !strings.Contains(err.Error(), `github app "b"`) || !strings.Contains(err.Error(), "injected encrypt failure") {
		t.Fatalf("partial seal: %v; want error naming app b", err)
	}
	assertSealed(t, base, "a", keys["a"], "enc:v1:static:")
	for _, name := range []string{"b", "c"} {
		if rawKey(t, base, name) != keys[name] {
			t.Fatalf("%s changed after failed seal", name)
		}
	}
	db := withCipher(base, static)
	if got, err := db.GetGitHubApp(ctx(), "a"); err != nil || got.PrivateKey != keys["a"] {
		t.Fatalf("already-sealed a unreadable: %v", err)
	}
	apps, err := db.LoadGitHubApps(ctx())
	if err != nil || len(apps) != 3 {
		t.Fatalf("retry load: %d apps, %v", len(apps), err)
	}
	for _, a := range apps {
		if a.PrivateKey != keys[a.Name] {
			t.Fatalf("%s key mismatch after retry", a.Name)
		}
	}
}

func TestGitHubAppKeysConcurrentLoadSealsOnce(t *testing.T) {
	base := isolatedDB(t)
	keys := seedPlaintext(t, base, "a", "b", "c", "d")
	c := &countingCipher{Cipher: staticCipher(t, 1)}
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i := range errs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			apps, err := withCipher(base, c).LoadGitHubApps(ctx())
			if err == nil && len(apps) != len(keys) {
				err = fmt.Errorf("%d apps", len(apps))
			}
			errs[i] = err
		}()
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if n := c.encrypts.Load(); n != int64(len(keys)) {
		t.Fatalf("%d encrypts for %d legacy rows; want each sealed once", n, len(keys))
	}
}

func TestGitHubAppKeysLockTimeout(t *testing.T) {
	base := isolatedDB(t)
	seedPlaintext(t, base, "a")
	holder, err := base.Pool.Acquire(ctx())
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Release()
	key := pg.LockKey(githubAppKeysLockName)
	if _, err := holder.Exec(ctx(), `SELECT pg_advisory_lock($1)`, key); err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = holder.Exec(ctx(), `SELECT pg_advisory_unlock($1)`, key) }()
	prev := githubAppKeysLockWait
	githubAppKeysLockWait = 300 * time.Millisecond
	t.Cleanup(func() { githubAppKeysLockWait = prev })

	start := time.Now()
	_, err = withCipher(base, staticCipher(t, 1)).LoadGitHubApps(ctx())
	if err == nil || !strings.Contains(err.Error(), "not acquired within") {
		t.Fatalf("held lock: %v; want a lock timeout error", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatal("lock wait not bounded")
	}
	if _, err := withCipher(base, keycrypt.None{}).LoadGitHubApps(ctx()); err != nil {
		t.Fatalf("none mode should not take the lock: %v", err)
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
