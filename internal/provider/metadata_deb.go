package provider

import (
	"context"
	"time"
)

// DebMetadataReader is the read surface the deb LocalIndexer needs to
// regenerate a flat apt repository (Packages/Release) from stored rows.
// *database.DB satisfies it; ServeLocalIndex type-asserts the FileStore to it,
// mirroring how the rpm provider reaches its RPMMetadataReader.
type DebMetadataReader interface {
	ListDebMetadataEntries(ctx context.Context, repoName string) ([]DebMetadata, error)
}

// DebMetadata is the derived per-package metadata for a Debian .deb, carrying
// the full raw control stanza so the Packages index can be regenerated
// faithfully alongside the computed size/md5/sha256 apt requires.
type DebMetadata struct {
	RepoName     string
	FilePath     string
	ContentHash  string
	Name         string
	Version      string
	Architecture string
	Control      string
	Size         int64
	MD5          string
	SHA256       string
	// CreatedAt is the persisted insert time; the Release Date: is derived from
	// the newest value so the index is byte-identical across replicas and
	// regenerations (issue #117) rather than stamped from wall clock.
	CreatedAt time.Time
}
