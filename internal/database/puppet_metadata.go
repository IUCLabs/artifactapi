package database

import (
	"context"

	"github.com/IUCLabs/artifactapi/internal/provider"
)

func (db *DB) InsertPuppetMetadata(ctx context.Context, meta *provider.PuppetMetadata) error {
	_, err := db.Pool.Exec(ctx, `
		INSERT INTO puppet_metadata (repo_name, file_path, content_hash, slug, version, metadata_json, file_size, file_md5)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8)
		ON CONFLICT (repo_name, file_path) DO NOTHING
	`, meta.RepoName, meta.FilePath, meta.ContentHash, meta.Slug, meta.Version, meta.MetadataJSON, meta.Size, meta.MD5)
	return err
}

func (db *DB) DeletePuppetMetadata(ctx context.Context, repoName, filePath string) error {
	_, err := db.Pool.Exec(ctx, `DELETE FROM puppet_metadata WHERE repo_name = $1 AND file_path = $2`, repoName, filePath)
	return err
}

// ListPuppetMetadataEntries returns a repo's releases, optionally narrowed to
// one module slug (empty slug lists every module).
func (db *DB) ListPuppetMetadataEntries(ctx context.Context, repoName, slug string) ([]provider.PuppetMetadata, error) {
	rows, err := db.Pool.Query(ctx, `
		SELECT repo_name, file_path, content_hash, slug, version, metadata_json, file_size, file_md5, created_at
		FROM puppet_metadata
		WHERE repo_name = $1 AND ($2 = '' OR slug = $2)
		ORDER BY slug, file_path
	`, repoName, slug)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var result []provider.PuppetMetadata
	for rows.Next() {
		var m provider.PuppetMetadata
		if err := rows.Scan(&m.RepoName, &m.FilePath, &m.ContentHash, &m.Slug, &m.Version, &m.MetadataJSON, &m.Size, &m.MD5, &m.CreatedAt); err != nil {
			return nil, err
		}
		result = append(result, m)
	}
	return result, rows.Err()
}
