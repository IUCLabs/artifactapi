package database

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/IUCLabs/golib/pg"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/IUCLabs/artifactapi/internal/keycrypt"
	"github.com/IUCLabs/artifactapi/pkg/models"
)

const githubAppKeysLockName = "artifactapi-github-app-keys"

var (
	githubAppKeysLockWait  = time.Minute
	githubAppKeysLockRetry = 200 * time.Millisecond
)

const githubAppKeysUnlockWait = 5 * time.Second

// LoadGitHubApps decrypts every stored GitHub App key, failing on the first
// app whose key cannot be decrypted. With encryption enabled it first
// re-seals, under an advisory lock, every key not stored in the current mode
// (legacy plaintext or the other cipher). Mode none refuses ciphertext rows.
func (db *DB) LoadGitHubApps(ctx context.Context) ([]models.GitHubApp, error) {
	if db.keyCipher().Mode() == keycrypt.ModeNone {
		var name string
		err := db.Pool.QueryRow(ctx, `SELECT name FROM github_apps WHERE private_key LIKE 'enc:%' ORDER BY name LIMIT 1`).Scan(&name)
		if err == nil {
			return nil, fmt.Errorf("github app %q: private key is encrypted at rest but GITHUB_APP_KEY_ENCRYPTION=none; "+
				"rollback to none is not supported, configure the cipher that sealed it", name)
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return nil, err
		}
		return db.loadGitHubApps(ctx, db.Pool)
	}

	conn, err := db.lockGitHubAppKeys(ctx)
	if err != nil {
		return nil, err
	}
	defer db.unlockGitHubAppKeys(ctx, conn)
	if err := db.sealKeys(ctx, conn.Conn()); err != nil {
		return nil, err
	}
	return db.loadGitHubApps(ctx, conn)
}

type rowsQuerier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

func (db *DB) loadGitHubApps(ctx context.Context, q rowsQuerier) ([]models.GitHubApp, error) {
	rows, err := q.Query(ctx, `SELECT `+githubAppCols+` FROM github_apps ORDER BY name`)
	if err != nil {
		return nil, err
	}
	apps, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (models.GitHubApp, error) {
		var a models.GitHubApp
		return a, scanGitHubApp(r, &a)
	})
	if err != nil {
		return nil, err
	}
	for i := range apps {
		if err := db.openKey(ctx, &apps[i]); err != nil {
			return nil, err
		}
	}
	return apps, nil
}

func (db *DB) lockGitHubAppKeys(ctx context.Context) (*pgxpool.Conn, error) {
	conn, err := db.Pool.Acquire(ctx)
	if err != nil {
		return nil, err
	}
	key := pg.LockKey(githubAppKeysLockName)
	wait, cancel := context.WithTimeout(ctx, githubAppKeysLockWait)
	defer cancel()
	for {
		var ok bool
		if err := conn.QueryRow(wait, `SELECT pg_try_advisory_lock($1)`, key).Scan(&ok); err != nil {
			conn.Release()
			if wait.Err() != nil {
				return nil, fmt.Errorf("github app keys lock: not acquired within %s", githubAppKeysLockWait)
			}
			return nil, fmt.Errorf("github app keys lock: %w", err)
		}
		if ok {
			return conn, nil
		}
		select {
		case <-wait.Done():
			conn.Release()
			return nil, fmt.Errorf("github app keys lock: not acquired within %s (another replica is sealing keys)", githubAppKeysLockWait)
		case <-time.After(githubAppKeysLockRetry):
		}
	}
}

// unlockGitHubAppKeys releases the lock, or closes the connection if the
// unlock fails so it never returns to the pool still holding the lock.
func (db *DB) unlockGitHubAppKeys(ctx context.Context, conn *pgxpool.Conn) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), githubAppKeysUnlockWait)
	defer cancel()
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_unlock($1)`, pg.LockKey(githubAppKeysLockName)); err != nil {
		slog.Warn("github app keys unlock failed; closing connection", "err", err)
		_ = conn.Hijack().Close(ctx)
		return
	}
	conn.Release()
}

// sealKeys re-encrypts, in the current mode, every key stored as legacy
// plaintext or under another configured cipher. The cipher round-trip runs
// outside any transaction; each row is swapped only if it still holds the
// value that was read, so a concurrent write wins and is left as is.
func (db *DB) sealKeys(ctx context.Context, conn *pgx.Conn) error {
	keys := db.keyCipher()
	mode := keys.Mode()
	rows, err := conn.Query(ctx, `SELECT name, private_key FROM github_apps WHERE private_key NOT LIKE $1 ORDER BY name`, "enc:v1:"+mode+":%")
	if err != nil {
		return err
	}
	type row struct{ name, stored string }
	pending, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (row, error) {
		var x row
		return x, r.Scan(&x.name, &x.stored)
	})
	if err != nil {
		return err
	}
	checker, _ := keys.(interface{ CanDecrypt(string) error })
	for _, r := range pending {
		if checker != nil {
			if err := checker.CanDecrypt(r.stored); err != nil {
				return fmt.Errorf("github app %q: %w", r.name, err)
			}
		} else if _, err := keycrypt.StoredMode(r.stored); err != nil {
			return fmt.Errorf("github app %q: %w", r.name, err)
		}
	}
	for i := range pending {
		r := &pending[i]
		from, _ := keycrypt.StoredMode(r.stored)
		swapped, err := db.resealKey(ctx, conn, r.name, r.stored, from)
		r.stored = ""
		if err != nil {
			return fmt.Errorf("github app %q: re-seal private key as %s (apps before it are already sealed): %w", r.name, mode, err)
		}
		if swapped {
			slog.Info("github app private key sealed at rest", "name", r.name, "from", from, "mode", mode)
		} else {
			slog.Info("github app private key changed while sealing; keeping the new value", "name", r.name)
		}
	}
	return nil
}

// resealKey encrypts stored in the current mode and swaps it in if the row
// still holds stored; updated_at is left alone.
func (db *DB) resealKey(ctx context.Context, conn *pgx.Conn, name, stored, from string) (bool, error) {
	a := models.GitHubApp{Name: name, PrivateKey: stored}
	if from != keycrypt.ModeNone {
		if err := db.openKey(ctx, &a); err != nil {
			return false, err
		}
	}
	sealed, err := db.sealKey(ctx, &a)
	a.PrivateKey = ""
	if err != nil {
		return false, err
	}
	tag, err := conn.Exec(ctx, `UPDATE github_apps SET private_key = $3 WHERE name = $1 AND private_key = $2`, name, stored, sealed)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}
