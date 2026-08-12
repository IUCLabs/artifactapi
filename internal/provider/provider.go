package provider

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/IUCLabs/artifactapi/pkg/models"
)

type Mutability int

const (
	Immutable Mutability = iota
	Mutable
)

type Provider interface {
	Type() models.PackageType
	Classify(path string) Mutability
	ContentType(path string) string
	UpstreamURL(remote models.Remote, path string) string
	RewriteResponse(body []byte, remote models.Remote, proxyBaseURL string) ([]byte, error)
	AuthHeaders(ctx context.Context, remote models.Remote) (http.Header, error)
}

type FileEntry struct {
	FilePath    string
	ContentHash string
}

type FileStore interface {
	ListFilesByPrefix(ctx context.Context, repoName, prefix string) ([]FileEntry, error)
	ListPackages(ctx context.Context, repoName string) ([]string, error)
}

type LocalUploader interface {
	ValidateUpload(filePath string) (storagePath, contentType string, err error)
	UploadResponse(storagePath, contentHash string, sizeBytes int64) map[string]any
}

type LocalIndexer interface {
	ServeLocalIndex(w http.ResponseWriter, r *http.Request, files FileStore, repoName, path string) bool
	GenerateLocalIndex(ctx context.Context, files FileStore, repoName, path string) ([]byte, error)
}

type BlobReader interface {
	Download(ctx context.Context, key string) (io.ReadCloser, int64, error)
}

type PostUploadHook interface {
	AfterUpload(ctx context.Context, repoName, storagePath, contentHash string, blobs BlobReader, db MetadataStore)
}

// PostDeleteHook lets a provider clean up derived state (e.g. RPM metadata that
// feeds generated repodata) after a local file is removed.
type PostDeleteHook interface {
	AfterDelete(ctx context.Context, repoName, storagePath string, db MetadataDeleter) error
}

type MetadataStore interface {
	InsertRPMMetadata(ctx context.Context, meta *RPMMetadata) error
	InsertDebMetadata(ctx context.Context, meta *DebMetadata) error
}

// RemoteServer lets a remote provider fully answer a request itself instead of
// going through the byte-proxy engine. It is the remote-side analog of
// LocalIndexer: a metadata-only remote (e.g. github_rpm) uses it to synthesize
// repodata from derived per-asset metadata and to redirect package downloads to
// a backend remote, without ever precaching the packages. Returning false lets
// the normal proxy path take over.
type RemoteServer interface {
	ServeRemote(w http.ResponseWriter, r *http.Request, remote models.Remote, path, proxyBaseURL string, store RemoteMetadataStore) bool
}

// RemoteMetadataStore is the persistence surface a RemoteServer needs to cache
// and read the metadata it derives per upstream asset. *database.DB satisfies it.
type RemoteMetadataStore interface {
	RPMMetadataReader
	MetadataStore
	MetadataDeleter
}

type MetadataDeleter interface {
	DeleteRPMMetadata(ctx context.Context, repoName, filePath string) error
	DeleteDebMetadata(ctx context.Context, repoName, filePath string) error
}

type RPMMetadataReader interface {
	ListRPMMetadataEntries(ctx context.Context, repoName string) ([]RPMMetadata, error)
}

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
}

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

type IndexMerger interface {
	MergeIndexes(members []MemberIndex, proxyBaseURL string) ([]byte, error)
}

type MemberIndex struct {
	RemoteName string
	Body       []byte
}

var registry = map[models.PackageType]Provider{}

func Register(p Provider) {
	registry[p.Type()] = p
}

func Get(t models.PackageType) (Provider, error) {
	p, ok := registry[t]
	if !ok {
		return nil, fmt.Errorf("no provider registered for package type %q", t)
	}
	return p, nil
}

func All() map[models.PackageType]Provider {
	return registry
}
