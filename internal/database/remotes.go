package database

import (
	"context"

	"github.com/jackc/pgx/v5"

	"github.com/IUCLabs/artifactapi/pkg/models"
)

const remoteCols = `name, package_type, repo_type, base_url, mirrorlist, mirror_strategy, description, username, password,
	immutable_ttl, mutable_ttl, check_mutable,
	patterns, blocklist, mutable_patterns, immutable_patterns,
	ban_tags_enabled, ban_tags,
	quarantine_enabled, quarantine_days, stale_on_error,
	releases_remote, managed_by,
	upstream_dial_timeout, upstream_tls_timeout, upstream_response_header_timeout,
	created_at, updated_at`

// normalizeMirrorStrategy maps an empty strategy to the round_robin default so
// the NOT NULL mirror_strategy column always stores a canonical value.
func normalizeMirrorStrategy(s string) string {
	if s == "" {
		return models.MirrorStrategyRoundRobin
	}
	return s
}

func scanRemote(scanner interface{ Scan(...any) error }, r *models.Remote) error {
	return scanner.Scan(
		&r.Name, &r.PackageType, &r.RepoType, &r.BaseURL, &r.Mirrorlist, &r.MirrorStrategy, &r.Description, &r.Username, &r.Password,
		&r.ImmutableTTL, &r.MutableTTL, &r.CheckMutable,
		&r.Patterns, &r.Blocklist, &r.MutablePatterns, &r.ImmutablePatterns,
		&r.BanTagsEnabled, &r.BanTags,
		&r.QuarantineEnabled, &r.QuarantineDays, &r.StaleOnError,
		&r.ReleasesRemote, &r.ManagedBy,
		&r.UpstreamDialTimeout, &r.UpstreamTLSTimeout, &r.UpstreamResponseHeaderTimeout,
		&r.CreatedAt, &r.UpdatedAt,
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
			name, package_type, repo_type, base_url, mirrorlist, description, username, password,
			immutable_ttl, mutable_ttl, check_mutable,
			patterns, blocklist, mutable_patterns, immutable_patterns,
			ban_tags_enabled, ban_tags,
			quarantine_enabled, quarantine_days, stale_on_error,
			releases_remote, managed_by,
			upstream_dial_timeout, upstream_tls_timeout, upstream_response_header_timeout,
			mirror_strategy
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23,$24,$25,$26)
	`,
		r.Name, r.PackageType, r.RepoType, r.BaseURL, r.Mirrorlist, r.Description, r.Username, r.Password,
		r.ImmutableTTL, r.MutableTTL, r.CheckMutable,
		r.Patterns, r.Blocklist, r.MutablePatterns, r.ImmutablePatterns,
		r.BanTagsEnabled, r.BanTags,
		r.QuarantineEnabled, r.QuarantineDays, r.StaleOnError,
		r.ReleasesRemote, r.ManagedBy,
		r.UpstreamDialTimeout, r.UpstreamTLSTimeout, r.UpstreamResponseHeaderTimeout,
		normalizeMirrorStrategy(r.MirrorStrategy),
	)
	return err
}

func (db *DB) UpdateRemote(ctx context.Context, r *models.Remote) error {
	_, err := db.Pool.Exec(ctx, `
		UPDATE remotes SET
			package_type=$2, repo_type=$3, base_url=$4, mirrorlist=$25, description=$5, username=$6, password=$7,
			immutable_ttl=$8, mutable_ttl=$9, check_mutable=$10,
			patterns=$11, blocklist=$12, mutable_patterns=$13, immutable_patterns=$14,
			ban_tags_enabled=$15, ban_tags=$16,
			quarantine_enabled=$17, quarantine_days=$18, stale_on_error=$19,
			releases_remote=$20, managed_by=$21,
			upstream_dial_timeout=$22, upstream_tls_timeout=$23, upstream_response_header_timeout=$24,
			mirror_strategy=$26,
			updated_at=NOW()
		WHERE name=$1
	`,
		r.Name, r.PackageType, r.RepoType, r.BaseURL, r.Description, r.Username, r.Password,
		r.ImmutableTTL, r.MutableTTL, r.CheckMutable,
		r.Patterns, r.Blocklist, r.MutablePatterns, r.ImmutablePatterns,
		r.BanTagsEnabled, r.BanTags,
		r.QuarantineEnabled, r.QuarantineDays, r.StaleOnError,
		r.ReleasesRemote, r.ManagedBy,
		r.UpstreamDialTimeout, r.UpstreamTLSTimeout, r.UpstreamResponseHeaderTimeout,
		r.Mirrorlist,
		normalizeMirrorStrategy(r.MirrorStrategy),
	)
	return err
}

// DeleteRemote drops a remote with its local files and package metadata, so a
// repo recreated under the same name starts empty.
func (db *DB) DeleteRemote(ctx context.Context, name string) error {
	return pgx.BeginFunc(ctx, db.Pool, func(tx pgx.Tx) error {
		for _, table := range []string{"local_files", "rpm_metadata", "deb_metadata", "alpine_metadata", "puppet_metadata", "helm_metadata"} {
			if _, err := tx.Exec(ctx, `DELETE FROM `+table+` WHERE repo_name = $1`, name); err != nil {
				return err
			}
		}
		_, err := tx.Exec(ctx, `DELETE FROM remotes WHERE name = $1`, name)
		return err
	})
}
