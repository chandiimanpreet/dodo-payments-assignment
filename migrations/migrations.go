// Package migrations embeds the SQL files so the api binary can run them at startup.
package migrations

import "embed"

//go:embed *.sql
var FS embed.FS
