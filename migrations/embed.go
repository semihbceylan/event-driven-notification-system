// Package migrations embeds the versioned SQL migrations into the binary,
// so the service can bring the schema up to date on startup.
package migrations

import "embed"

//go:embed *.sql
var FS embed.FS
