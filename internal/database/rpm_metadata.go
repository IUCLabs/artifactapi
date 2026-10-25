package database

import (
	"context"
	"encoding/json"
	"time"

	"github.com/IUCLabs/artifactapi/internal/provider"
)

func (db *DB) InsertRPMMetadata(ctx context.Context, meta *provider.RPMMetadata) error {
	requiresJSON, _ := json.Marshal(meta.Requires)
	providesJSON, _ := json.Marshal(meta.Provides)
	conflictsJSON, _ := json.Marshal(meta.Conflicts)
	obsoletesJSON, _ := json.Marshal(meta.Obsoletes)
	filesJSON, _ := json.Marshal(meta.Files)
	changelogsJSON, _ := json.Marshal(meta.Changelogs)

	_, err := db.Pool.Exec(ctx, `
		INSERT INTO rpm_metadata (
			repo_name, file_path, content_hash,
			name, epoch, version, release, arch,
			summary, description, rpm_size, installed_size,
			license, vendor, build_group, build_host, source_rpm, url, packager,
			requires, provides, conflicts, obsoletes, files, changelogs
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23,$24,$25)
		ON CONFLICT (repo_name, file_path) DO NOTHING
	`,
		meta.RepoName, meta.FilePath, meta.ContentHash,
		meta.Name, meta.Epoch, meta.Version, meta.Release, meta.Arch,
		meta.Summary, meta.Description, meta.RPMSize, meta.InstalledSize,
		meta.License, meta.Vendor, meta.Group, meta.BuildHost, meta.SourceRPM, meta.URL, meta.Packager,
		requiresJSON, providesJSON, conflictsJSON, obsoletesJSON, filesJSON, changelogsJSON,
	)
	return err
}

func (db *DB) DeleteRPMMetadata(ctx context.Context, repoName, filePath string) error {
	_, err := db.Pool.Exec(ctx, `DELETE FROM rpm_metadata WHERE repo_name = $1 AND file_path = $2`, repoName, filePath)
	return err
}

// RPMMetadataFingerprint is a cheap change detector for a repo's rows: they are
// only ever inserted or deleted, so (count, max id) moves whenever they change.
// For github_rpm remotes it relies on the sync lease serialising scans.
func (db *DB) RPMMetadataFingerprint(ctx context.Context, repoName string) (count, maxID int64, err error) {
	err = db.Pool.QueryRow(ctx, `SELECT count(*), coalesce(max(id), 0) FROM rpm_metadata WHERE repo_name = $1`, repoName).Scan(&count, &maxID)
	return count, maxID, err
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
	Conflicts     json.RawMessage
	Obsoletes     json.RawMessage
	Files         json.RawMessage
	Changelogs    json.RawMessage
	CreatedAt     time.Time
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
			CreatedAt:     r.CreatedAt,
		}
		json.Unmarshal(r.Requires, &meta.Requires)
		json.Unmarshal(r.Provides, &meta.Provides)
		json.Unmarshal(r.Conflicts, &meta.Conflicts)
		json.Unmarshal(r.Obsoletes, &meta.Obsoletes)
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
			requires, provides, conflicts, obsoletes, files, changelogs,
			created_at
		FROM rpm_metadata
		WHERE repo_name = $1
		ORDER BY name, epoch, version, release, arch, file_path
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
			&r.Requires, &r.Provides, &r.Conflicts, &r.Obsoletes, &r.Files, &r.Changelogs,
			&r.CreatedAt,
		); err != nil {
			return nil, err
		}
		result = append(result, r)
	}
	return result, rows.Err()
}
