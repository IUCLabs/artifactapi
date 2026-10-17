//go:build dockere2e

package e2edocker

func init() {
	cachingCases = append(cachingCases, cachingCase{"nuget", "nuget/e2epkg/1.0.0/e2epkg.1.0.0.nupkg", "nuget/e2epkg/1.0.0/e2epkg.1.0.0.nupkg"})
}
