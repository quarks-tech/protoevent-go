// Package tidbtest boots an ephemeral TiDB (testcontainers) with the outbox
// schema applied, for integration tests.
package tidbtest

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	_ "github.com/go-sql-driver/mysql"
	"github.com/golang-migrate/migrate/v4"
	migratemysql "github.com/golang-migrate/migrate/v4/database/mysql"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	"github.com/moby/moby/api/types/network"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	tidb "github.com/quarks-tech/protoevent-go/pkg/transport/outbox/tidb"
)

const (
	tidbImage = "pingcap/tidb:v7.5.1"
	dbName    = "outbox_test"
	startupTO = 180 * time.Second
	tidbPort  = "4000/tcp"
)

type Instance struct {
	DB        *sql.DB
	DSN       string
	terminate func()
}

// Start boots TiDB, creates the db, applies migrations, and returns a ready
// Instance + cleanup. Returns an error (tests should t.Skip on it) when Docker
// is unavailable.
func Start(ctx context.Context) (*Instance, func(), error) {
	req := testcontainers.ContainerRequest{
		Image:        tidbImage,
		ExposedPorts: []string{tidbPort},
		WaitingFor: wait.ForSQL(tidbPort, "mysql", func(host string, port network.Port) string {
			return fmt.Sprintf("root:@tcp(%s:%s)/", host, port.Port())
		}).WithStartupTimeout(startupTO),
	}
	c, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: req, Started: true,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("start tidb (Docker unavailable?): %w", err)
	}
	host, err := c.Host(ctx)
	if err != nil {
		_ = c.Terminate(ctx)
		return nil, nil, err
	}
	mapped, err := c.MappedPort(ctx, tidbPort)
	if err != nil {
		_ = c.Terminate(ctx)
		return nil, nil, err
	}
	base := fmt.Sprintf("root:@tcp(%s:%s)/", host, mapped.Port())

	admin, err := sql.Open("mysql", base)
	if err != nil {
		_ = c.Terminate(ctx)
		return nil, nil, err
	}
	if _, err := admin.ExecContext(ctx, "CREATE DATABASE IF NOT EXISTS "+dbName); err != nil {
		_ = admin.Close()
		_ = c.Terminate(ctx)
		return nil, nil, err
	}
	_ = admin.Close()

	// tidb_skip_isolation_level_check=1: golang-migrate's mysql driver runs
	// migrations inside a sql.LevelSerializable transaction; TiDB rejects
	// SERIALIZABLE unless this session variable is set.
	dsn := base + dbName + "?parseTime=true&loc=UTC&multiStatements=true&tidb_skip_isolation_level_check=1"
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		_ = c.Terminate(ctx)
		return nil, nil, err
	}

	src, err := iofs.New(tidb.Migrations, "migrations")
	if err != nil {
		_ = db.Close()
		_ = c.Terminate(ctx)
		return nil, nil, err
	}
	drv, err := migratemysql.WithInstance(db, &migratemysql.Config{DatabaseName: dbName})
	if err != nil {
		_ = db.Close()
		_ = c.Terminate(ctx)
		return nil, nil, err
	}
	m, err := migrate.NewWithInstance("iofs", src, "mysql", drv)
	if err != nil {
		_ = db.Close()
		_ = c.Terminate(ctx)
		return nil, nil, err
	}
	if err := m.Up(); err != nil && err != migrate.ErrNoChange {
		_ = db.Close()
		_ = c.Terminate(ctx)
		return nil, nil, err
	}

	inst := &Instance{DB: db, DSN: dsn, terminate: func() { _ = db.Close(); _ = c.Terminate(context.Background()) }}
	return inst, inst.terminate, nil
}
