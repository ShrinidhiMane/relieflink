// Package db embeds the SQL migrations and CSV seed data so the binary is
// self-contained. The same files can be run by hand with psql (see scripts/).
package db

import "embed"

//go:embed migrations/*.sql
var Migrations embed.FS

//go:embed seed/*.sql seed/*.csv
var Seed embed.FS
