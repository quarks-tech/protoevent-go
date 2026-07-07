package tidb

import "embed"

// Migrations holds the outbox schema migrations (golang-migrate iofs source).
//
//go:embed migrations/*.sql
var Migrations embed.FS
