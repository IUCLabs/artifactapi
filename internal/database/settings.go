package database

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/IUCLabs/artifactapi/pkg/models"
)

const selectSettings = `
	SELECT s3_health_up_threshold, s3_health_down_threshold, s3_health_interval_ms, s3_health_timeout_ms
	FROM settings WHERE id`

type querier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

func scanSettings(row pgx.Row) (models.Settings, error) {
	var s models.Settings
	err := row.Scan(&s.S3HealthUpThreshold, &s.S3HealthDownThreshold, &s.S3HealthIntervalMs, &s.S3HealthTimeoutMs)
	if errors.Is(err, pgx.ErrNoRows) {
		return models.Settings{}, nil
	}
	return s, err
}

// GetSettings returns the stored overrides; with no row every field is nil.
func (db *DB) GetSettings(ctx context.Context) (models.Settings, error) {
	return scanSettings(db.Pool.QueryRow(ctx, selectSettings))
}

// PutSettings replaces every override; nil fields are stored as NULL.
func (db *DB) PutSettings(ctx context.Context, s models.Settings) error {
	return putSettings(ctx, db.Pool, s)
}

// UpdateSettings stores fn's result over the stored overrides under a row lock, so concurrent
// updates serialise; an error from fn aborts the update and is returned as is.
func (db *DB) UpdateSettings(ctx context.Context, fn func(models.Settings) (models.Settings, error)) error {
	return pgx.BeginFunc(ctx, db.Pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO settings (id) VALUES (TRUE) ON CONFLICT (id) DO NOTHING`); err != nil {
			return err
		}
		stored, err := scanSettings(tx.QueryRow(ctx, selectSettings+" FOR UPDATE"))
		if err != nil {
			return err
		}
		next, err := fn(stored)
		if err != nil {
			return err
		}
		return putSettings(ctx, tx, next)
	})
}

func putSettings(ctx context.Context, q querier, s models.Settings) error {
	_, err := q.Exec(ctx, `
		INSERT INTO settings (id, s3_health_up_threshold, s3_health_down_threshold, s3_health_interval_ms, s3_health_timeout_ms)
		VALUES (TRUE, $1, $2, $3, $4)
		ON CONFLICT (id) DO UPDATE SET
			s3_health_up_threshold=$1, s3_health_down_threshold=$2,
			s3_health_interval_ms=$3, s3_health_timeout_ms=$4, updated_at=NOW()
	`, s.S3HealthUpThreshold, s.S3HealthDownThreshold, s.S3HealthIntervalMs, s.S3HealthTimeoutMs)
	return err
}

// ResetSettings clears every override.
func (db *DB) ResetSettings(ctx context.Context) error {
	return db.PutSettings(ctx, models.Settings{})
}
