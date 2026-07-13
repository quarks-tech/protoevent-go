// Package tidbtest boots an ephemeral TiDB (testcontainers) with the outbox
// schema applied, for integration tests.
package tidbtest

import (
	"context"
	"database/sql"
	"errors"
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
	tidbImage      = "pingcap/tidb:v7.5.1"
	dbName         = "outbox_test"
	startupTimeout = 180 * time.Second
	tidbPort       = "4000/tcp"
)

// ErrDockerUnavailable marks errors caused by Docker/the container runtime
// being unavailable, as opposed to a genuine harness bug (e.g. a bad DSN, a
// failed migration, or a driver API mismatch). Callers should use errors.Is
// against this sentinel to decide whether a Start failure is an acceptable
// skip or a real failure.
var ErrDockerUnavailable = errors.New("docker unavailable")

type Instance struct {
	DB  *sql.DB
	DSN string
}

// Start boots TiDB, creates the db, applies migrations, and returns a ready
// Instance + cleanup. Returns an error wrapping ErrDockerUnavailable (callers
// should use errors.Is against it to decide whether to skip) when Docker/the
// container runtime is unavailable; any other error is a genuine harness bug
// and callers should fail loudly on it.
func Start(ctx context.Context) (*Instance, func(), error) {
	// Probe the Docker daemon explicitly BEFORE starting a container, so only
	// a genuinely-unavailable daemon maps to ErrDockerUnavailable; any error
	// from the container start/migrations below (with a healthy daemon) is a
	// real harness bug and must fail loudly, not be silently skipped.
	if err := probeDocker(ctx); err != nil {
		return nil, nil, fmt.Errorf("probe docker daemon: %w", errors.Join(ErrDockerUnavailable, err))
	}

	req := testcontainers.ContainerRequest{
		Image:        tidbImage,
		ExposedPorts: []string{tidbPort},
		WaitingFor: wait.ForSQL(tidbPort, "mysql", func(host string, port network.Port) string {
			return fmt.Sprintf("root:@tcp(%s:%s)/", host, port.Port())
		}).WithStartupTimeout(startupTimeout),
	}
	c, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: req, Started: true,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("start tidb container: %w", err)
	}

	// db is captured by cleanup below; nil until the connection is opened
	// further down, so an early failure's cleanup only terminates the
	// container.
	var db *sql.DB

	// cleanup is disarmed (set to nil) once Start succeeds; until then it
	// unwinds whatever has been created so far on any error return. One
	// closure for the whole function (rather than a second one constructed
	// after the connection opens) — it closes db only once db is non-nil.
	cleanup := func() {
		if db != nil {
			_ = db.Close()
		}
		_ = c.Terminate(context.Background())
	}
	defer func() {
		if cleanup != nil {
			cleanup()
		}
	}()

	host, err := c.Host(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("get tidb container host: %w", err)
	}
	mapped, err := c.MappedPort(ctx, tidbPort)
	if err != nil {
		return nil, nil, fmt.Errorf("get tidb mapped port: %w", err)
	}
	base := fmt.Sprintf("root:@tcp(%s:%s)/", host, mapped.Port())

	admin, err := sql.Open("mysql", base)
	if err != nil {
		return nil, nil, fmt.Errorf("open admin connection: %w", err)
	}
	_, execErr := admin.ExecContext(ctx, "CREATE DATABASE IF NOT EXISTS "+dbName)
	_ = admin.Close()
	if execErr != nil {
		return nil, nil, fmt.Errorf("create database %s: %w", dbName, execErr)
	}

	// tidb_skip_isolation_level_check=1: golang-migrate's mysql driver runs
	// migrations inside a sql.LevelSerializable transaction; TiDB rejects
	// SERIALIZABLE unless this session variable is set.
	dsn := base + dbName + "?parseTime=true&loc=UTC&multiStatements=true&tidb_skip_isolation_level_check=1"
	db, err = sql.Open("mysql", dsn)
	if err != nil {
		return nil, nil, fmt.Errorf("open tidb connection: %w", err)
	}

	src, err := iofs.New(tidb.Migrations, "migrations")
	if err != nil {
		return nil, nil, fmt.Errorf("open migrations source: %w", err)
	}
	drv, err := migratemysql.WithInstance(db, &migratemysql.Config{DatabaseName: dbName})
	if err != nil {
		return nil, nil, fmt.Errorf("create migrate driver: %w", err)
	}
	m, err := migrate.NewWithInstance("iofs", src, "mysql", drv)
	if err != nil {
		return nil, nil, fmt.Errorf("create migrator: %w", err)
	}
	if err := m.Up(); err != nil && err != migrate.ErrNoChange {
		return nil, nil, fmt.Errorf("apply migrations: %w", err)
	}

	inst := &Instance{DB: db, DSN: dsn}
	terminate := cleanup
	cleanup = nil // disarm the deferred unwind: ownership passes to the caller
	return inst, terminate, nil
}

// probeDocker reports whether the Docker daemon is reachable, via the
// testcontainers Docker provider's own health check (client Info call). The
// provider is closed by Health itself.
func probeDocker(ctx context.Context) error {
	provider, err := testcontainers.NewDockerProvider()
	if err != nil {
		return fmt.Errorf("create docker provider: %w", err)
	}
	if err := provider.Health(ctx); err != nil {
		return fmt.Errorf("docker daemon health check: %w", err)
	}
	return nil
}
