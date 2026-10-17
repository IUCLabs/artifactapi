package provider

import (
	"context"
	"time"
)

type HelmMetadataStore interface {
	InsertHelmMetadata(ctx context.Context, meta *HelmMetadata) error
}

type HelmMetadataDeleter interface {
	DeleteHelmMetadata(ctx context.Context, repoName, filePath string) error
}

type HelmMetadataReader interface {
	ListHelmMetadataEntries(ctx context.Context, repoName string) ([]HelmMetadata, error)
}

// HelmMetadata is a local chart's Chart.yaml, kept verbatim so index.yaml
// entries carry every field helm reads.
type HelmMetadata struct {
	RepoName    string
	FilePath    string
	ContentHash string
	Name        string
	Version     string
	ChartYAML   string
	CreatedAt   time.Time
}
