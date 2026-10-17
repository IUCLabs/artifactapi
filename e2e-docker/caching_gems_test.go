//go:build dockere2e

package e2edocker

func init() {
	cachingCases = append(cachingCases, cachingCase{"gems", "gems/mygem-1.0.0.gem", "gems/mygem-1.0.0.gem"})
}
