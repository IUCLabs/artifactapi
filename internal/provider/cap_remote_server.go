package provider

import (
	"net/http"

	"github.com/IUCLabs/artifactapi/pkg/models"
)

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
