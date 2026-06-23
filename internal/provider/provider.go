package provider

import (
	"context"
	"fmt"
	"io"
	"net/http"

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

type MetadataStore interface {
	InsertRPMMetadata(ctx context.Context, meta *RPMMetadata) error
}

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
	Files         []RPMFile
	Changelogs    []RPMChangelog
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
