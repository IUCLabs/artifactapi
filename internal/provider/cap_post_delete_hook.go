package provider

import (
	"context"
)

// PostDeleteHook lets a provider clean up derived state (e.g. RPM metadata that
// feeds generated repodata) after a local file is removed.
type PostDeleteHook interface {
	AfterDelete(ctx context.Context, repoName, storagePath string, db MetadataDeleter) error
}
