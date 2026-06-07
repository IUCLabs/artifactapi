package models

import "time"

type LocalFile struct {
	ID          int64     `json:"id"`
	RepoName    string    `json:"repo_name"`
	FilePath    string    `json:"file_path"`
	ContentHash string    `json:"content_hash"`
	CreatedAt   time.Time `json:"created_at"`
}
