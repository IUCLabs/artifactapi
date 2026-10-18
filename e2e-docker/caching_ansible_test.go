//go:build dockere2e

package e2edocker

func init() {
	cachingCases = append(cachingCases, cachingCase{"ansible", "galaxy/artifacts/ns-col-1.0.0.tar.gz", "galaxy/artifacts/ns-col-1.0.0.tar.gz"})
}
