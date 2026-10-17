package provider

import (
	"context"
	"time"
)

type PuppetMetadataStore interface {
	InsertPuppetMetadata(ctx context.Context, meta *PuppetMetadata) error
}

type PuppetMetadataDeleter interface {
	DeletePuppetMetadata(ctx context.Context, repoName, filePath string) error
}

type PuppetMetadataReader interface {
	ListPuppetMetadataEntries(ctx context.Context, repoName, slug string) ([]PuppetMetadata, error)
}

// PuppetMetadata is a local module release; MetadataJSON is the tarball's
// metadata.json verbatim so Forge release objects carry every field.
type PuppetMetadata struct {
	RepoName     string
	FilePath     string
	ContentHash  string
	Slug         string
	Version      string
	MetadataJSON string
	Size         int64
	MD5          string
	CreatedAt    time.Time
}
