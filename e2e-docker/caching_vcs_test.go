//go:build dockere2e

package e2edocker

func init() {
	cachingCases = append(cachingCases, cachingCase{"vcs", "octo/repo/archive/refs/tags/v1.0.0.tar.gz", "octo/repo/archive/refs/tags/v1.0.0.tar.gz"})
}
