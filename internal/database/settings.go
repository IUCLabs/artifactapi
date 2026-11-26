package database

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"

	"github.com/IUCLabs/artifactapi/pkg/models"
)

// GetSettings returns the stored overrides; with no row every field is nil.
func (db *DB) GetSettings(ctx context.Context) (models.Settings, error) {
	var s models.Settings
	err := db.Pool.QueryRow(ctx, `
		SELECT s3_health_up_threshold, s3_health_down_threshold, s3_health_interval_ms, s3_health_timeout_ms
		FROM settings WHERE id
	`).Scan(&s.S3HealthUpThreshold, &s.S3HealthDownThreshold, &s.S3HealthIntervalMs, &s.S3HealthTimeoutMs)
	if errors.Is(err, pgx.ErrNoRows) {
		return models.Settings{}, nil
	}
	return s, err
}

// PutSettings replaces every override; nil fields are stored as NULL.
func (db *DB) PutSettings(ctx context.Context, s models.Settings) error {
	_, err := db.Pool.Exec(ctx, `
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
