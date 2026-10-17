package provider

import (
	"context"
)

type MetadataStore interface {
	InsertRPMMetadata(ctx context.Context, meta *RPMMetadata) error
	InsertDebMetadata(ctx context.Context, meta *DebMetadata) error
}

type MetadataDeleter interface {
	DeleteRPMMetadata(ctx context.Context, repoName, filePath string) error
	DeleteDebMetadata(ctx context.Context, repoName, filePath string) error
}
