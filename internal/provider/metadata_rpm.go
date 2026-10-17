package provider

import (
	"context"
	"time"
)

type RPMMetadataReader interface {
	ListRPMMetadataEntries(ctx context.Context, repoName string) ([]RPMMetadata, error)
}

type RPMMetadata struct {
	RepoName      string
	FilePath      string
	ContentHash   string
	Name          string
	Epoch         int
	Version       string
	Release       string
	Arch          string
	Summary       string
	Description   string
	RPMSize       int64
	InstalledSize int64
	License       string
	Vendor        string
	Group         string
	BuildHost     string
	SourceRPM     string
	URL           string
	Packager      string
	Requires      []RPMDep
	Provides      []RPMDep
	Conflicts     []RPMDep
	Obsoletes     []RPMDep
	Files         []RPMFile
	Changelogs    []RPMChangelog
	// CreatedAt is the persisted upload timestamp; used as a stable, replica-independent
	// value for the repodata <time>/<revision> fields so generated indexes are deterministic.
	CreatedAt time.Time
}

type RPMDep struct {
	Name    string `json:"name"`
	Flags   string `json:"flags,omitempty"`
	Epoch   string `json:"epoch,omitempty"`
	Version string `json:"version,omitempty"`
	Release string `json:"release,omitempty"`
}

type RPMFile struct {
	Path string `json:"path"`
	Type string `json:"type,omitempty"`
}

type RPMChangelog struct {
	Author string `json:"author"`
	Date   int64  `json:"date"`
	Text   string `json:"text"`
}
