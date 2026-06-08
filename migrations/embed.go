// Package migrations embeds the goose SQL migration files so they can be applied
// programmatically on startup without shipping the .sql files separately.
package migrations

import "embed"

//go:embed *.sql
var FS embed.FS
