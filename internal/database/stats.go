package database

import (
	"context"

	"github.com/IUCLabs/artifactapi/pkg/models"
)

func (db *DB) GetOverviewStats(ctx context.Context) (*models.OverviewStats, error) {
	var stats models.OverviewStats

	err := db.Pool.QueryRow(ctx, `SELECT COUNT(*) FROM remotes`).Scan(&stats.TotalRemotes)
	if err != nil {
		return nil, err
	}

	err = db.Pool.QueryRow(ctx, `SELECT COALESCE(COUNT(*), 0), COALESCE(SUM(b.size_bytes), 0)
		FROM artifacts a JOIN blobs b ON a.content_hash = b.content_hash`).
		Scan(&stats.TotalObjects, &stats.TotalBytes)
	if err != nil {
		return nil, err
	}

	err = db.Pool.QueryRow(ctx, `
		SELECT COALESCE(
			(SELECT COUNT(*) FROM artifacts) - (SELECT COUNT(DISTINCT content_hash) FROM artifacts),
			0
		)`).Scan(&stats.TotalBlobsDeduped)
	if err != nil {
		return nil, err
	}

	return &stats, nil
}

type RemoteStatRow struct {
	Name        string `json:"name"`
	ObjectCount int64  `json:"object_count"`
	TotalBytes  int64  `json:"total_bytes"`
	Requests30d int64  `json:"requests_30d"`
}

func (db *DB) GetTopRemotes(ctx context.Context, limit int) ([]RemoteStatRow, error) {
	rows, err := db.Pool.Query(ctx, `
		SELECT r.name,
			COALESCE(a.cnt, 0) AS object_count,
			COALESCE(a.total_bytes, 0) AS total_bytes,
			COALESCE(l.req_count, 0) AS requests_30d
		FROM remotes r
		LEFT JOIN (
			SELECT remote_name, COUNT(*) AS cnt, SUM(b.size_bytes) AS total_bytes
			FROM artifacts a JOIN blobs b ON a.content_hash = b.content_hash
			GROUP BY remote_name
		) a ON r.name = a.remote_name
		LEFT JOIN (
			SELECT remote_name, COUNT(*) AS req_count
			FROM access_log
			WHERE created_at > NOW() - INTERVAL '30 days'
			GROUP BY remote_name
		) l ON r.name = l.remote_name
		ORDER BY COALESCE(a.total_bytes, 0) DESC
		LIMIT $1
	`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var result []RemoteStatRow
	for rows.Next() {
		var r RemoteStatRow
		if err := rows.Scan(&r.Name, &r.ObjectCount, &r.TotalBytes, &r.Requests30d); err != nil {
			return nil, err
		}
		result = append(result, r)
	}
	return result, rows.Err()
}

type FileStatRow struct {
	RemoteName  string `json:"remote_name"`
	Path        string `json:"path"`
	AccessCount int64  `json:"access_count"`
	SizeBytes   int64  `json:"size_bytes"`
}

func (db *DB) GetTopFilesByHits(ctx context.Context, limit int) ([]FileStatRow, error) {
	rows, err := db.Pool.Query(ctx, `
		SELECT a.remote_name, a.path, a.access_count, b.size_bytes
		FROM artifacts a
		JOIN blobs b ON a.content_hash = b.content_hash
		ORDER BY a.access_count DESC
		LIMIT $1
	`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var result []FileStatRow
	for rows.Next() {
		var r FileStatRow
		if err := rows.Scan(&r.RemoteName, &r.Path, &r.AccessCount, &r.SizeBytes); err != nil {
			return nil, err
		}
		result = append(result, r)
	}
	return result, rows.Err()
}

type BandwidthStatRow struct {
	RemoteName string `json:"remote_name"`
	Path       string `json:"path"`
	Bandwidth  int64  `json:"bandwidth"`
	Requests   int64  `json:"requests"`
}

func (db *DB) GetTopFilesByBandwidth(ctx context.Context, limit int) ([]BandwidthStatRow, error) {
	rows, err := db.Pool.Query(ctx, `
		SELECT remote_name, path,
			COALESCE(SUM(size_bytes), 0) AS bandwidth,
			COUNT(*) AS requests
		FROM access_log
		WHERE created_at > NOW() - INTERVAL '30 days'
		GROUP BY remote_name, path
		ORDER BY bandwidth DESC
		LIMIT $1
	`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var result []BandwidthStatRow
	for rows.Next() {
		var r BandwidthStatRow
		if err := rows.Scan(&r.RemoteName, &r.Path, &r.Bandwidth, &r.Requests); err != nil {
			return nil, err
		}
		result = append(result, r)
	}
	return result, rows.Err()
}
