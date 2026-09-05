// Package migrations embeds the SQL schema files so artifactapi carries its own
// schema and applies it at startup, with no externally mirrored copy to drift
// out of sync.
package migrations

import "embed"

// FS holds every numbered migration; lexical filename order is version order.
//
//go:embed *.sql
var FS embed.FS
