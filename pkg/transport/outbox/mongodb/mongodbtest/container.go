// Package mongodbtest boots an ephemeral single-node MongoDB replica set
// (testcontainers) for integration tests: change streams and transactions both
// require a replica set.
package mongodbtest

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/mongodb"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

const dbName = "outbox_test"

// ErrDockerUnavailable marks errors caused by Docker/the container runtime
// being unavailable, as opposed to a genuine harness bug (e.g. a bad DSN or a
// driver API mismatch). Callers should use errors.Is against this sentinel to
// decide whether a Start failure is an acceptable skip or a real failure.
var ErrDockerUnavailable = errors.New("docker unavailable")

type Instance struct {
	Client    *mongo.Client
	DB        *mongo.Database
	terminate func()
}

// Start boots MongoDB as a single-node replica set, connects, and returns a
// ready Instance + cleanup. Returns an error (tests should t.Skip on it) when
// Docker is unavailable.
func Start(ctx context.Context) (*Instance, func(), error) {
	// Probe the Docker daemon explicitly BEFORE starting a container, so only
	// a genuinely-unavailable daemon maps to ErrDockerUnavailable; any error
	// from the container start below (with a healthy daemon) — e.g. an
	// image-pull failure or a replica-set init bug — is a real harness bug
	// and must fail loudly, not be silently skipped.
	if err := probeDocker(ctx); err != nil {
		return nil, nil, fmt.Errorf("probe docker daemon: %w", errors.Join(ErrDockerUnavailable, err))
	}

	c, err := mongodb.Run(ctx, "mongo:8", mongodb.WithReplicaSet("rs0"))
	if err != nil {
		return nil, nil, fmt.Errorf("start mongodb: %w", err)
	}
	// cleanup paths always run on a FRESH bounded context: on the error paths
	// the caller's ctx may already be canceled (Terminate would no-op and leak
	// the container), and an unbounded Background() could hang a wedged
	// daemon's teardown indefinitely.
	terminate := func() {
		tctx, tcancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer tcancel()
		_ = c.Terminate(tctx)
	}

	uri, err := c.ConnectionString(ctx)
	if err != nil {
		terminate()
		return nil, nil, fmt.Errorf("get connection string: %w", err)
	}
	// directConnection: the single-node RS advertises a container-internal
	// address; a direct connection to the mapped port still supports txns and
	// change streams (the server IS a replica-set member).
	sep := "&"
	if !strings.Contains(uri, "?") {
		sep = "?"
	}
	dsn := uri + sep + "directConnection=true"
	client, err := mongo.Connect(options.Client().ApplyURI(dsn)) // v2: no ctx arg
	if err != nil {
		terminate()
		return nil, nil, fmt.Errorf("connect: %w", err)
	}
	disconnect := func() {
		dctx, dcancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer dcancel()
		_ = client.Disconnect(dctx)
	}
	// Ping so a bad DSN (or a container that never becomes reachable) fails
	// loudly here, at Start, with a clear error — instead of surfacing as a
	// confusing timeout on the caller's first real operation. A ping failure
	// after the container itself started successfully is a genuine harness
	// bug, not a missing-Docker condition, so it is NOT wrapped in
	// ErrDockerUnavailable.
	pingCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := client.Ping(pingCtx, nil); err != nil {
		disconnect()
		terminate()
		return nil, nil, fmt.Errorf("ping mongodb: %w", err)
	}
	inst := &Instance{
		Client:    client,
		DB:        client.Database(dbName),
		terminate: func() { disconnect(); terminate() },
	}
	return inst, inst.terminate, nil
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
