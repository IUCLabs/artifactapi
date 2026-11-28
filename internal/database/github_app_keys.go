package database

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/IUCLabs/golib/pg"
	"github.com/jackc/pgx/v5"

	"github.com/IUCLabs/artifactapi/internal/keycrypt"
	"github.com/IUCLabs/artifactapi/pkg/models"
)

const githubAppKeysLockName = "artifactapi-github-app-keys"

// LoadGitHubApps decrypts every stored GitHub App key, failing on the first
// app whose key cannot be decrypted. With encryption enabled it first seals
// legacy plaintext keys in place, under an advisory lock so replicas don't race.
func (db *DB) LoadGitHubApps(ctx context.Context) ([]models.GitHubApp, error) {
	conn, err := db.Pool.Acquire(ctx)
	if err != nil {
		return nil, err
	}
	defer conn.Release()
	key := pg.LockKey(githubAppKeysLockName)
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, key); err != nil {
		return nil, fmt.Errorf("github app keys lock: %w", err)
	}
	defer func() {
		if _, err := conn.Exec(context.WithoutCancel(ctx), `SELECT pg_advisory_unlock($1)`, key); err != nil {
			slog.Warn("github app keys unlock failed", "err", err)
		}
	}()

	if db.keyCipher().Mode() != keycrypt.ModeNone {
		if err := db.sealLegacyKeys(ctx, conn.Conn()); err != nil {
			return nil, err
		}
	}

	rows, err := conn.Query(ctx, `SELECT `+githubAppCols+` FROM github_apps ORDER BY name`)
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

func (db *DB) sealLegacyKeys(ctx context.Context, conn *pgx.Conn) error {
	rows, err := conn.Query(ctx, `SELECT name FROM github_apps WHERE private_key NOT LIKE 'enc:v1:%' ORDER BY name`)
	if err != nil {
		return err
	}
	names, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return err
	}
	for _, name := range names {
		if err := pgx.BeginFunc(ctx, conn, func(tx pgx.Tx) error {
			var stored string
			err := tx.QueryRow(ctx, `SELECT private_key FROM github_apps WHERE name = $1 FOR UPDATE`, name).Scan(&stored)
			if errors.Is(err, pgx.ErrNoRows) || (err == nil && !keycrypt.IsLegacy(stored)) {
				return nil
			}
			if err != nil {
				return err
			}
			sealed, err := db.sealKey(ctx, &models.GitHubApp{Name: name, PrivateKey: stored})
			if err != nil {
				return err
			}
			_, err = tx.Exec(ctx, `UPDATE github_apps SET private_key = $2 WHERE name = $1`, name, sealed)
			return err
		}); err != nil {
			return fmt.Errorf("github app %q: seal legacy private key: %w", name, err)
		}
		slog.Info("github app private key encrypted at rest", "name", name, "mode", db.keyCipher().Mode())
	}
	return nil
}
