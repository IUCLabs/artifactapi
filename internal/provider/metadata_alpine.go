package provider

import (
	"context"
)

// AlpineMetadataStore / AlpineMetadataDeleter / AlpineMetadataReader are the
// Alpine-specific persistence surfaces. They are kept separate from the shared
// RPM/Deb metadata interfaces so the apk provider can type-assert the generic
// MetadataStore/MetadataDeleter/FileStore it is handed without widening (and
// thus perturbing the test doubles of) the rpm and deb providers. *database.DB
// satisfies all three.
type AlpineMetadataStore interface {
	InsertAlpineMetadata(ctx context.Context, meta *AlpineMetadata) error
}

type AlpineMetadataDeleter interface {
	DeleteAlpineMetadata(ctx context.Context, repoName, filePath string) error
}

type AlpineMetadataReader interface {
	ListAlpineMetadataEntries(ctx context.Context, repoName string) ([]AlpineMetadata, error)
}

// AlpineMetadata is the derived per-package metadata for an Alpine .apk, holding
// the fields an APKINDEX record carries plus the apk pull checksum (Q1…, the
// sha1 of the control gzip stream) and the download/installed sizes.
type AlpineMetadata struct {
	RepoName         string
	FilePath         string
	ContentHash      string
	Checksum         string   // C: "Q1" + base64(sha1(control gzip stream))
	Name             string   // P:
	Version          string   // V:
	Arch             string   // A:
	DownloadSize     int64    // S: on-disk .apk size
	InstalledSize    int64    // I: unpacked size from .PKGINFO
	Description      string   // T:
	URL              string   // U:
	License          string   // L:
	Origin           string   // o:
	Maintainer       string   // m:
	BuildTime        int64    // t:
	Commit           string   // c:
	ProviderPriority string   // k:
	Depends          []string // D:
	Provides         []string // p:
	InstallIf        []string // i:
}
