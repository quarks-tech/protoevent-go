// Package tidbmigrate applies the outbox schema with golang-migrate, hiding
// the source/driver/instance boilerplate every consumer would otherwise
// repeat. It is a separate package so that publish-only builds importing
// tidb never pull the migrate machinery into their binaries.
package tidbmigrate

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"

	"github.com/golang-migrate/migrate/v4"
	migratemysql "github.com/golang-migrate/migrate/v4/database/mysql"
	"github.com/golang-migrate/migrate/v4/source/iofs"

	"github.com/quarks-tech/protoevent-go/pkg/transport/outbox/tidb"
)

// config carries the migration options.
type config struct {
	prefix    string
	prefixSet bool // WithTablePrefix was called; "" then validates as invalid instead of meaning "default"
}

// Option configures Apply.
type Option func(*config)

// WithTablePrefix migrates a prefixed outbox instance (see
// tidb.WithTablePrefix): the DDL is rewritten via tidb.PrefixedMigrations,
// and the golang-migrate versions table becomes prefix+"schema_migrations"
// automatically — each instance MUST track its own versions, or instances in
// one schema silently skip each other's DDL.
func WithTablePrefix(prefix string) Option {
	return func(c *config) { c.prefix, c.prefixSet = prefix, true }
}

// Apply applies the outbox schema (all pending versions) to db. It is
// idempotent: an already-up-to-date schema is not an error. The DSN must
// allow multiStatements=true, and TiDB needs
// tidb_skip_isolation_level_check=1 (golang-migrate runs its transaction at
// SERIALIZABLE, which TiDB rejects without it).
//
// The prefix (if any) is validated by tidb.PrefixedMigrations; an invalid one
// returns an error rather than panicking here — unlike the store options,
// this runs at operational time where an error channel exists.
func Apply(db *sql.DB, opts ...Option) error {
	var c config
	for _, opt := range opts {
		opt(&c)
	}

	var (
		source   fs.FS = tidb.Migrations
		mysqlCfg       = migratemysql.Config{}
	)
	// Gate on "the option was called", not on prefix != "": an explicitly
	// passed empty prefix must fail PrefixedMigrations' validation loudly, not
	// silently degrade into migrating the DEFAULT (unprefixed) instance.
	if c.prefixSet {
		prefixed, err := tidb.PrefixedMigrations(c.prefix)
		if err != nil {
			return fmt.Errorf("outbox: migrate: %w", err)
		}
		source = prefixed
		mysqlCfg.MigrationsTable = c.prefix + "schema_migrations"
	}

	src, err := iofs.New(source, "migrations")
	if err != nil {
		return fmt.Errorf("outbox: migrate: open source: %w", err)
	}
	// Build the driver over a dedicated conn via WithConnection, NOT
	// WithInstance: WithInstance adopts the caller's *sql.DB, so the migrator's
	// Close would close db itself out from under the application. With a conn
	// we checked out ourselves, Close only returns that conn to db's pool.
	ctx := context.Background()
	conn, err := db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("outbox: migrate: acquire conn: %w", err)
	}
	drv, err := migratemysql.WithConnection(ctx, conn, &mysqlCfg)
	if err != nil {
		_ = conn.Close()
		return fmt.Errorf("outbox: migrate: create driver: %w", err)
	}
	m, err := migrate.NewWithInstance("iofs", src, "mysql", drv)
	if err != nil {
		_ = conn.Close()
		return fmt.Errorf("outbox: migrate: create migrator: %w", err)
	}
	defer func() { _, _ = m.Close() }()
	if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return fmt.Errorf("outbox: migrate: apply: %w", err)
	}
	return nil
}
