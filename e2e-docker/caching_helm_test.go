//go:build dockere2e

package e2edocker

func init() {
	cachingCases = append(cachingCases, cachingCase{"helm", "charts/mychart-1.0.0.tgz", "charts/mychart-1.0.0.tgz"})
}
