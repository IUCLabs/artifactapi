//go:build dockere2e

package e2edocker

func init() {
	cachingCases = append(cachingCases, cachingCase{"rpm", "rpmrepo/Packages/e2e-testpkg-1.0-1.noarch.rpm", "rpmrepo/Packages/e2e-testpkg-1.0-1.noarch.rpm"})
}
