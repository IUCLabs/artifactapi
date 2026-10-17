//go:build dockere2e

package e2edocker

func init() {
	cachingCases = append(cachingCases, cachingCase{"generic", "blobs/hello.bin", "blobs/hello.bin"})
}
