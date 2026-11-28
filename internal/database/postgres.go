package database

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/IUCLabs/golib/pg"

	"github.com/IUCLabs/artifactapi/internal/keycrypt"
	"github.com/IUCLabs/artifactapi/migrations"
)

// migrationLockName names the cluster-wide advisory lock the migration run
// contends for; golib derives the key as FNV-1a/64 of it. Replicas starting
// together queue on it instead of racing each other through the DDL.
const migrationLockName = "artifactapi-migrations"

type DB struct {
	Pool *pgxpool.Pool
	keys keycrypt.Cipher
}

// SetKeyCipher sets how GitHub App private keys are sealed at rest. Call
// before use; unset means plaintext (keycrypt.None).
func (db *DB) SetKeyCipher(c keycrypt.Cipher) { db.keys = c }

func (db *DB) keyCipher() keycrypt.Cipher {
	if db.keys == nil {
		return keycrypt.None{}
	}
	return db.keys
}

func New(dsn string) (*DB, error) {
	ctx := context.Background()
	pool, err := pg.NewMigrated(ctx, dsn, migrations.FS, pg.MigrateOptions{
		LockName: migrationLockName,
	})
	if err != nil {
		return nil, err
	}
	return &DB{Pool: pool}, nil
}

func (db *DB) Close() {
	db.Pool.Close()
}
