package tidb

import (
	"embed"
	"fmt"
	"io/fs"
	"strings"
	"testing/fstest"
)

// Migrations holds the outbox schema migrations (golang-migrate iofs source)
// for the DEFAULT (unprefixed) table names. For a WithTablePrefix instance,
// use PrefixedMigrations.
//
//go:embed migrations/*.sql
var Migrations embed.FS

// PrefixedMigrations returns Migrations with every outbox table name rewritten
// to carry prefix, for a WithTablePrefix outbox instance. The result is a
// golang-migrate iofs source, same as Migrations (files live under
// "migrations/").
//
// IMPORTANT: golang-migrate records applied versions in ONE table per
// database (default "schema_migrations"), so two outbox instances migrating
// in the same schema would collide on version numbers and silently skip each
// other's DDL. Give each instance its own versions table:
//
//	drv, err := mysql.WithInstance(db, &mysql.Config{
//	    MigrationsTable: "orders_schema_migrations", // prefix + "schema_migrations"
//	})
func PrefixedMigrations(prefix string) (fs.FS, error) {
	if err := validateTablePrefix(prefix); err != nil {
		return nil, err
	}
	replacer := strings.NewReplacer(
		baseMessagesTable, prefix+baseMessagesTable,
		baseOffsetsTable, prefix+baseOffsetsTable,
		baseSequencersTable, prefix+baseSequencersTable,
		baseLocksTable, prefix+baseLocksTable,
	)
	out := fstest.MapFS{}
	err := fs.WalkDir(Migrations, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		b, err := Migrations.ReadFile(path)
		if err != nil {
			return fmt.Errorf("read embedded migration %s: %w", path, err)
		}
		out[path] = &fstest.MapFile{Data: []byte(replacer.Replace(string(b)))}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("outbox: prefixed migrations: %w", err)
	}
	return out, nil
}
