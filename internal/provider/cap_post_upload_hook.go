package provider

import (
	"context"
)

type PostUploadHook interface {
	AfterUpload(ctx context.Context, repoName, storagePath, contentHash string, blobs BlobReader, db MetadataStore)
}
