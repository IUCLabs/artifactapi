//go:build dockere2e

package e2edocker

func init() {
	cachingCases = append(cachingCases, cachingCase{"puppet", "puppet-releases/author-mod-1.0.0.tar.gz", "puppet-releases/author-mod-1.0.0.tar.gz"})
}
