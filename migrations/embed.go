// Package migrations embeds the SQL schema migrations so the binary can
// apply them on startup.
package migrations

import "embed"

//go:embed *.sql
var FS embed.FS
