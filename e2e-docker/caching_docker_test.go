//go:build dockere2e

package e2edocker

func init() {
	cachingCases = append(cachingCases, cachingCase{"docker", "library/testimg/blobs/blobdata", "v2/library/testimg/blobs/blobdata"})
}
