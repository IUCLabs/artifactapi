//go:build dockere2e

package e2edocker

func init() {
	cachingCases = append(cachingCases, cachingCase{"cargo", "crates/mycrate/mycrate-1.0.0.crate", "crates/mycrate/mycrate-1.0.0.crate"})
}
