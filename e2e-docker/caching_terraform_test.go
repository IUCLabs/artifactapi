//go:build dockere2e

package e2edocker

func init() {
	cachingCases = append(cachingCases, cachingCase{"terraform", "hashicorp/aws/download/pkg.zip", "v1/providers/hashicorp/aws/download/pkg.zip"})
}
