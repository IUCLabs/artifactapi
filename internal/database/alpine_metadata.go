package database

import (
	"context"
	"strings"

	"github.com/IUCLabs/artifactapi/internal/provider"
)

func (db *DB) InsertAlpineMetadata(ctx context.Context, meta *provider.AlpineMetadata) error {
	_, err := db.Pool.Exec(ctx, `
		INSERT INTO alpine_metadata (
			repo_name, file_path, content_hash, checksum,
			name, version, arch, download_size, installed_size,
			description, url, license, origin, maintainer,
			build_time, commit_hash, provider_priority,
			depends, provides, install_if
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20)
		ON CONFLICT (repo_name, file_path) DO NOTHING
	`,
		meta.RepoName, meta.FilePath, meta.ContentHash, meta.Checksum,
		meta.Name, meta.Version, meta.Arch, meta.DownloadSize, meta.InstalledSize,
		meta.Description, meta.URL, meta.License, meta.Origin, meta.Maintainer,
		meta.BuildTime, meta.Commit, meta.ProviderPriority,
		strings.Join(meta.Depends, " "), strings.Join(meta.Provides, " "), strings.Join(meta.InstallIf, " "),
	)
	return err
}

func (db *DB) DeleteAlpineMetadata(ctx context.Context, repoName, filePath string) error {
	_, err := db.Pool.Exec(ctx, `DELETE FROM alpine_metadata WHERE repo_name = $1 AND file_path = $2`, repoName, filePath)
	return err
}

func (db *DB) ListAlpineMetadataEntries(ctx context.Context, repoName string) ([]provider.AlpineMetadata, error) {
	rows, err := db.Pool.Query(ctx, `
		SELECT repo_name, file_path, content_hash, checksum,
			name, version, arch, download_size, installed_size,
			description, url, license, origin, maintainer,
			build_time, commit_hash, provider_priority,
			depends, provides, install_if
		FROM alpine_metadata
		WHERE repo_name = $1
		ORDER BY name, version, arch
	`, repoName)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var result []provider.AlpineMetadata
	for rows.Next() {
		var m provider.AlpineMetadata
		var depends, provides, installIf string
		if err := rows.Scan(
			&m.RepoName, &m.FilePath, &m.ContentHash, &m.Checksum,
			&m.Name, &m.Version, &m.Arch, &m.DownloadSize, &m.InstalledSize,
			&m.Description, &m.URL, &m.License, &m.Origin, &m.Maintainer,
			&m.BuildTime, &m.Commit, &m.ProviderPriority,
			&depends, &provides, &installIf,
		); err != nil {
			return nil, err
		}
		m.Depends = strings.Fields(depends)
		m.Provides = strings.Fields(provides)
		m.InstallIf = strings.Fields(installIf)
		result = append(result, m)
	}
	return result, rows.Err()
}
