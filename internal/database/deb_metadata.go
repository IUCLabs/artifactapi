package database

import (
	"context"

	"github.com/IUCLabs/artifactapi/internal/provider"
)

func (db *DB) InsertDebMetadata(ctx context.Context, meta *provider.DebMetadata) error {
	_, err := db.Pool.Exec(ctx, `
		INSERT INTO deb_metadata (
			repo_name, file_path, content_hash,
			name, version, architecture, control,
			size, md5, sha256
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)
		ON CONFLICT (repo_name, file_path) DO NOTHING
	`,
		meta.RepoName, meta.FilePath, meta.ContentHash,
		meta.Name, meta.Version, meta.Architecture, meta.Control,
		meta.Size, meta.MD5, meta.SHA256,
	)
	return err
}

func (db *DB) DeleteDebMetadata(ctx context.Context, repoName, filePath string) error {
	_, err := db.Pool.Exec(ctx, `DELETE FROM deb_metadata WHERE repo_name = $1 AND file_path = $2`, repoName, filePath)
	return err
}

func (db *DB) ListDebMetadataEntries(ctx context.Context, repoName string) ([]provider.DebMetadata, error) {
	rows, err := db.Pool.Query(ctx, `
		SELECT repo_name, file_path, content_hash,
			name, version, architecture, control,
			size, md5, sha256, created_at
		FROM deb_metadata
		WHERE repo_name = $1
		ORDER BY name, version, architecture, file_path
	`, repoName)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var result []provider.DebMetadata
	for rows.Next() {
		var m provider.DebMetadata
		if err := rows.Scan(
			&m.RepoName, &m.FilePath, &m.ContentHash,
			&m.Name, &m.Version, &m.Architecture, &m.Control,
			&m.Size, &m.MD5, &m.SHA256, &m.CreatedAt,
		); err != nil {
			return nil, err
		}
		result = append(result, m)
	}
	return result, rows.Err()
}
