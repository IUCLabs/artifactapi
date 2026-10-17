//go:build dockere2e

package e2edocker

func init() {
	cachingCases = append(cachingCases, cachingCase{"goproxy", "goproxy/example.com/mod/@v/v1.0.0.zip", "goproxy/example.com/mod/@v/v1.0.0.zip"})
}
