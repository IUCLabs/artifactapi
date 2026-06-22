package database

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
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

func (db *DB) DeleteLocalFile(ctx context.Context, repoName, filePath string) error {
	_, err := db.Pool.Exec(ctx, `DELETE FROM local_files WHERE repo_name = $1 AND file_path = $2`, repoName, filePath)
	return err
}
