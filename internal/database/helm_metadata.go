package database

import (
	"context"

	"github.com/IUCLabs/artifactapi/internal/provider"
)

func (db *DB) InsertHelmMetadata(ctx context.Context, meta *provider.HelmMetadata) error {
	_, err := db.Pool.Exec(ctx, `
		INSERT INTO helm_metadata (repo_name, file_path, content_hash, name, version, chart_yaml)
		VALUES ($1,$2,$3,$4,$5,$6)
		ON CONFLICT (repo_name, file_path) DO NOTHING
	`, meta.RepoName, meta.FilePath, meta.ContentHash, meta.Name, meta.Version, meta.ChartYAML)
	return err
}

func (db *DB) DeleteHelmMetadata(ctx context.Context, repoName, filePath string) error {
	_, err := db.Pool.Exec(ctx, `DELETE FROM helm_metadata WHERE repo_name = $1 AND file_path = $2`, repoName, filePath)
	return err
}

func (db *DB) ListHelmMetadataEntries(ctx context.Context, repoName string) ([]provider.HelmMetadata, error) {
	rows, err := db.Pool.Query(ctx, `
		SELECT repo_name, file_path, content_hash, name, version, chart_yaml, created_at
		FROM helm_metadata
		WHERE repo_name = $1
		ORDER BY name, version, file_path
	`, repoName)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var result []provider.HelmMetadata
	for rows.Next() {
		var m provider.HelmMetadata
		if err := rows.Scan(&m.RepoName, &m.FilePath, &m.ContentHash, &m.Name, &m.Version, &m.ChartYAML, &m.CreatedAt); err != nil {
			return nil, err
		}
		result = append(result, m)
	}
	return result, rows.Err()
}
