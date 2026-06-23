package database

import (
	"context"
	"encoding/json"

	"github.com/IUCLabs/artifactapi/internal/provider"
)

func (db *DB) InsertRPMMetadata(ctx context.Context, meta *provider.RPMMetadata) error {
	requiresJSON, _ := json.Marshal(meta.Requires)
	providesJSON, _ := json.Marshal(meta.Provides)
	filesJSON, _ := json.Marshal(meta.Files)
	changelogsJSON, _ := json.Marshal(meta.Changelogs)

	_, err := db.Pool.Exec(ctx, `
		INSERT INTO rpm_metadata (
			repo_name, file_path, content_hash,
			name, epoch, version, release, arch,
			summary, description, rpm_size, installed_size,
			license, vendor, build_group, build_host, source_rpm, url, packager,
			requires, provides, files, changelogs
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23)
		ON CONFLICT (repo_name, file_path) DO NOTHING
	`,
		meta.RepoName, meta.FilePath, meta.ContentHash,
		meta.Name, meta.Epoch, meta.Version, meta.Release, meta.Arch,
		meta.Summary, meta.Description, meta.RPMSize, meta.InstalledSize,
		meta.License, meta.Vendor, meta.Group, meta.BuildHost, meta.SourceRPM, meta.URL, meta.Packager,
		requiresJSON, providesJSON, filesJSON, changelogsJSON,
	)
	return err
}

type RPMMetadataRow struct {
	RepoName      string
	FilePath      string
	ContentHash   string
	Name          string
	Epoch         int
	Version       string
	Release       string
	Arch          string
	Summary       string
	Description   string
	RPMSize       int64
	InstalledSize int64
	License       string
	Vendor        string
	Group         string
	BuildHost     string
	SourceRPM     string
	URL           string
	Packager      string
	Requires      json.RawMessage
	Provides      json.RawMessage
	Files         json.RawMessage
	Changelogs    json.RawMessage
}

func (db *DB) ListRPMMetadataEntries(ctx context.Context, repoName string) ([]provider.RPMMetadata, error) {
	rows, err := db.ListRPMMetadata(ctx, repoName)
	if err != nil {
		return nil, err
	}
	result := make([]provider.RPMMetadata, len(rows))
	for i, r := range rows {
		meta := provider.RPMMetadata{
			RepoName:      r.RepoName,
			FilePath:      r.FilePath,
			ContentHash:   r.ContentHash,
			Name:          r.Name,
			Epoch:         r.Epoch,
			Version:       r.Version,
			Release:       r.Release,
			Arch:          r.Arch,
			Summary:       r.Summary,
			Description:   r.Description,
			RPMSize:       r.RPMSize,
			InstalledSize: r.InstalledSize,
			License:       r.License,
			Vendor:        r.Vendor,
			Group:         r.Group,
			BuildHost:     r.BuildHost,
			SourceRPM:     r.SourceRPM,
			URL:           r.URL,
			Packager:      r.Packager,
		}
		json.Unmarshal(r.Requires, &meta.Requires)
		json.Unmarshal(r.Provides, &meta.Provides)
		json.Unmarshal(r.Files, &meta.Files)
		json.Unmarshal(r.Changelogs, &meta.Changelogs)
		result[i] = meta
	}
	return result, nil
}

func (db *DB) ListRPMMetadata(ctx context.Context, repoName string) ([]RPMMetadataRow, error) {
	rows, err := db.Pool.Query(ctx, `
		SELECT repo_name, file_path, content_hash,
			name, epoch, version, release, arch,
			summary, description, rpm_size, installed_size,
			license, vendor, build_group, build_host, source_rpm, url, packager,
			requires, provides, files, changelogs
		FROM rpm_metadata
		WHERE repo_name = $1
		ORDER BY name, epoch, version, release, arch
	`, repoName)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var result []RPMMetadataRow
	for rows.Next() {
		var r RPMMetadataRow
		if err := rows.Scan(
			&r.RepoName, &r.FilePath, &r.ContentHash,
			&r.Name, &r.Epoch, &r.Version, &r.Release, &r.Arch,
			&r.Summary, &r.Description, &r.RPMSize, &r.InstalledSize,
			&r.License, &r.Vendor, &r.Group, &r.BuildHost, &r.SourceRPM, &r.URL, &r.Packager,
			&r.Requires, &r.Provides, &r.Files, &r.Changelogs,
		); err != nil {
			return nil, err
		}
		result = append(result, r)
	}
	return result, rows.Err()
}
