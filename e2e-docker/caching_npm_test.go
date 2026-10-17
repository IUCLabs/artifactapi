//go:build dockere2e

package e2edocker

func init() {
	cachingCases = append(cachingCases, cachingCase{"npm", "mypkg/-/mypkg-1.0.0.tgz", "mypkg/-/mypkg-1.0.0.tgz"})
}
