package provider

import (
	"context"
	"net/http"
)

type LocalIndexer interface {
	ServeLocalIndex(w http.ResponseWriter, r *http.Request, files FileStore, repoName, path string) bool
	GenerateLocalIndex(ctx context.Context, files FileStore, repoName, path string) ([]byte, error)
}
