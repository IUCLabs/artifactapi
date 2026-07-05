package database

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/IUCLabs/artifactapi/internal/provider"
	"github.com/IUCLabs/artifactapi/pkg/models"
)

type LocalFile struct {
	ID          int64     `json:"id"`
	RepoName    string    `json:"repo_name"`
	FilePath    string    `json:"file_path"`
	ContentHash string    `json:"content_hash"`
	CreatedAt   time.Time `json:"created_at"`
}

var ErrAlreadyExists = fmt.Errorf("file already exists")

func (db *DB) CreateLocalFile(ctx context.Context, repoName, filePath, contentHash string) error {
	_, err := db.Pool.Exec(ctx, `
		INSERT INTO local_files (repo_name, file_path, content_hash)
		VALUES ($1, $2, $3)
	`, repoName, filePath, contentHash)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return ErrAlreadyExists
		}
		return err
	}
	return nil
}

// UpsertLocalFile inserts a local file or repoints an existing path at a new
// blob. Unlike CreateLocalFile it never errors on a duplicate path — it is for
// mutable references such as Docker tags, where re-pushing a tag must move it to
// the newly-pushed manifest rather than being rejected as an overwrite.
func (db *DB) UpsertLocalFile(ctx context.Context, repoName, filePath, contentHash string) error {
	_, err := db.Pool.Exec(ctx, `
		INSERT INTO local_files (repo_name, file_path, content_hash)
		VALUES ($1, $2, $3)
		ON CONFLICT (repo_name, file_path)
		DO UPDATE SET content_hash = EXCLUDED.content_hash, created_at = NOW()
	`, repoName, filePath, contentHash)
	return err
}

func (db *DB) GetLocalFile(ctx context.Context, repoName, filePath string) (*LocalFile, error) {
	row := db.Pool.QueryRow(ctx, `
		SELECT id, repo_name, file_path, content_hash, created_at
		FROM local_files
		WHERE repo_name = $1 AND file_path = $2
	`, repoName, filePath)

	var f LocalFile
	if err := row.Scan(&f.ID, &f.RepoName, &f.FilePath, &f.ContentHash, &f.CreatedAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return &f, nil
}

func (db *DB) ListLocalFiles(ctx context.Context, repoName string, limit, offset int) ([]LocalFile, error) {
	rows, err := db.Pool.Query(ctx, `
		SELECT id, repo_name, file_path, content_hash, created_at
		FROM local_files
		WHERE repo_name = $1
		ORDER BY file_path
		LIMIT $2 OFFSET $3
	`, repoName, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var files []LocalFile
	for rows.Next() {
		var f LocalFile
		if err := rows.Scan(&f.ID, &f.RepoName, &f.FilePath, &f.ContentHash, &f.CreatedAt); err != nil {
			return nil, err
		}
		files = append(files, f)
	}
	return files, rows.Err()
}

// ListLocalArtifacts returns a repo's local files shaped as models.Artifact so
// the UI's cached-objects view can render them the same way as remote artifacts.
// Local files carry no access/fetch counters, so those are left at zero and the
// timestamps are all derived from created_at.
func (db *DB) ListLocalArtifacts(ctx context.Context, repoName string, limit, offset int) ([]models.Artifact, error) {
	rows, err := db.Pool.Query(ctx, `
		SELECT lf.id, lf.repo_name, lf.file_path, lf.content_hash,
			lf.created_at, b.size_bytes, b.content_type
		FROM local_files lf
		JOIN blobs b ON lf.content_hash = b.content_hash
		WHERE lf.repo_name = $1
		ORDER BY lf.file_path
		LIMIT $2 OFFSET $3
	`, repoName, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var artifacts []models.Artifact
	for rows.Next() {
		var a models.Artifact
		var createdAt time.Time
		if err := rows.Scan(&a.ID, &a.RemoteName, &a.Path, &a.ContentHash, &createdAt, &a.SizeBytes, &a.ContentType); err != nil {
			return nil, err
		}
		a.FirstSeenAt = createdAt
		a.LastFetchedAt = createdAt
		a.LastAccessedAt = createdAt
		artifacts = append(artifacts, a)
	}
	return artifacts, rows.Err()
}

func (db *DB) ListLocalFilesByPrefix(ctx context.Context, repoName, prefix string) ([]LocalFile, error) {
	rows, err := db.Pool.Query(ctx, `
		SELECT id, repo_name, file_path, content_hash, created_at
		FROM local_files
		WHERE repo_name = $1 AND file_path LIKE $2
		ORDER BY file_path
	`, repoName, prefix+"%")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var files []LocalFile
	for rows.Next() {
		var f LocalFile
		if err := rows.Scan(&f.ID, &f.RepoName, &f.FilePath, &f.ContentHash, &f.CreatedAt); err != nil {
			return nil, err
		}
		files = append(files, f)
	}
	return files, rows.Err()
}

func (db *DB) ListLocalFilePackages(ctx context.Context, repoName string) ([]string, error) {
	rows, err := db.Pool.Query(ctx, `
		SELECT DISTINCT split_part(file_path, '/', 1)
		FROM local_files
		WHERE repo_name = $1
		ORDER BY 1
	`, repoName)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var packages []string
	for rows.Next() {
		var pkg string
		if err := rows.Scan(&pkg); err != nil {
			return nil, err
		}
		packages = append(packages, pkg)
	}
	return packages, rows.Err()
}

func (db *DB) ListFilesByPrefix(ctx context.Context, repoName, prefix string) ([]provider.FileEntry, error) {
	files, err := db.ListLocalFilesByPrefix(ctx, repoName, prefix)
	if err != nil {
		return nil, err
	}
	result := make([]provider.FileEntry, len(files))
	for i, f := range files {
		result[i] = provider.FileEntry{FilePath: f.FilePath, ContentHash: f.ContentHash}
	}
	return result, nil
}

func (db *DB) ListPackages(ctx context.Context, repoName string) ([]string, error) {
	return db.ListLocalFilePackages(ctx, repoName)
}

func (db *DB) DeleteLocalFile(ctx context.Context, repoName, filePath string) error {
	_, err := db.Pool.Exec(ctx, `DELETE FROM local_files WHERE repo_name = $1 AND file_path = $2`, repoName, filePath)
	return err
}
