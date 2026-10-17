package v1

import (
	"context"
	"encoding/json"

	"github.com/IUCLabs/artifactapi/internal/virtual"
)

// LocalNPMPackument serves a local npm repo's packument to npm virtuals, with
// tarball paths relative to the repo.
func (h *ProxyHandler) LocalNPMPackument(ctx context.Context, repo, pkg string) ([]byte, error) {
	files, err := h.db.ListLocalFilesByPrefix(ctx, repo, pkg+"/-/")
	if err != nil {
		return nil, err
	}
	read := func(hash string) ([]byte, error) { return h.readLocalBlob(ctx, hash) }
	doc, err := buildNPMPackument(pkg, files, read, "")
	if err != nil {
		return nil, err
	}
	if doc == nil {
		return nil, virtual.ErrNotFound
	}
	return json.Marshal(doc)
}
