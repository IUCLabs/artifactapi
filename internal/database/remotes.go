package database

import (
	"context"

	"github.com/IUCLabs/artifactapi/pkg/models"
)

const remoteCols = `name, package_type, base_url, description, username, password,
	immutable_ttl, mutable_ttl, check_mutable,
	patterns, blocklist, mutable_patterns, immutable_patterns,
	ban_tags_enabled, ban_tags,
	quarantine_enabled, quarantine_days, stale_on_error,
	releases_remote, managed_by, created_at, updated_at`

func scanRemote(scanner interface{ Scan(...any) error }, r *models.Remote) error {
	return scanner.Scan(
		&r.Name, &r.PackageType, &r.BaseURL, &r.Description, &r.Username, &r.Password,
		&r.ImmutableTTL, &r.MutableTTL, &r.CheckMutable,
		&r.Patterns, &r.Blocklist, &r.MutablePatterns, &r.ImmutablePatterns,
		&r.BanTagsEnabled, &r.BanTags,
		&r.QuarantineEnabled, &r.QuarantineDays, &r.StaleOnError,
		&r.ReleasesRemote, &r.ManagedBy, &r.CreatedAt, &r.UpdatedAt,
	)
}

func (db *DB) GetRemote(ctx context.Context, name string) (*models.Remote, error) {
	row := db.Pool.QueryRow(ctx, `SELECT `+remoteCols+` FROM remotes WHERE name = $1`, name)
	var r models.Remote
	if err := scanRemote(row, &r); err != nil {
		return nil, err
	}
	return &r, nil
}

func (db *DB) ListRemotes(ctx context.Context) ([]models.Remote, error) {
	rows, err := db.Pool.Query(ctx, `SELECT `+remoteCols+` FROM remotes ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var remotes []models.Remote
	for rows.Next() {
		var r models.Remote
		if err := scanRemote(rows, &r); err != nil {
			return nil, err
		}
		remotes = append(remotes, r)
	}
	return remotes, rows.Err()
}

func (db *DB) CreateRemote(ctx context.Context, r *models.Remote) error {
	_, err := db.Pool.Exec(ctx, `
		INSERT INTO remotes (
			name, package_type, base_url, description, username, password,
			immutable_ttl, mutable_ttl, check_mutable,
			patterns, blocklist, mutable_patterns, immutable_patterns,
			ban_tags_enabled, ban_tags,
			quarantine_enabled, quarantine_days, stale_on_error,
			releases_remote, managed_by
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20)
	`,
		r.Name, r.PackageType, r.BaseURL, r.Description, r.Username, r.Password,
		r.ImmutableTTL, r.MutableTTL, r.CheckMutable,
		r.Patterns, r.Blocklist, r.MutablePatterns, r.ImmutablePatterns,
		r.BanTagsEnabled, r.BanTags,
		r.QuarantineEnabled, r.QuarantineDays, r.StaleOnError,
		r.ReleasesRemote, r.ManagedBy,
	)
	return err
}

func (db *DB) UpdateRemote(ctx context.Context, r *models.Remote) error {
	_, err := db.Pool.Exec(ctx, `
		UPDATE remotes SET
			package_type=$2, base_url=$3, description=$4, username=$5, password=$6,
			immutable_ttl=$7, mutable_ttl=$8, check_mutable=$9,
			patterns=$10, blocklist=$11, mutable_patterns=$12, immutable_patterns=$13,
			ban_tags_enabled=$14, ban_tags=$15,
			quarantine_enabled=$16, quarantine_days=$17, stale_on_error=$18,
			releases_remote=$19, managed_by=$20, updated_at=NOW()
		WHERE name=$1
	`,
		r.Name, r.PackageType, r.BaseURL, r.Description, r.Username, r.Password,
		r.ImmutableTTL, r.MutableTTL, r.CheckMutable,
		r.Patterns, r.Blocklist, r.MutablePatterns, r.ImmutablePatterns,
		r.BanTagsEnabled, r.BanTags,
		r.QuarantineEnabled, r.QuarantineDays, r.StaleOnError,
		r.ReleasesRemote, r.ManagedBy,
	)
	return err
}

func (db *DB) DeleteRemote(ctx context.Context, name string) error {
	_, err := db.Pool.Exec(ctx, `DELETE FROM remotes WHERE name = $1`, name)
	return err
}
