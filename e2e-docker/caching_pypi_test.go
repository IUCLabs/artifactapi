//go:build dockere2e

package e2edocker

func init() {
	cachingCases = append(cachingCases, cachingCase{"pypi", "packages/foo-1.0-py3-none-any.whl", "packages/foo-1.0-py3-none-any.whl"})
}
