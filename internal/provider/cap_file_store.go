package provider

import (
	"context"
	"io"
)

type FileStore interface {
	ListFilesByPrefix(ctx context.Context, repoName, prefix string) ([]FileEntry, error)
	ListPackages(ctx context.Context, repoName string) ([]string, error)
}

type BlobReader interface {
	Download(ctx context.Context, key string) (io.ReadCloser, int64, error)
}
