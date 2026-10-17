//go:build dockere2e

package e2edocker

func init() {
	cachingCases = append(cachingCases, cachingCase{"alpine", "alpine/x86_64/testpkg-1.0-r0.apk", "alpine/x86_64/testpkg-1.0-r0.apk"})
}
