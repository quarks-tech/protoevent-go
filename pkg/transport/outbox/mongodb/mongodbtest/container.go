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

	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/testcontainers/testcontainers-go/modules/mongodb"
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
	c, err := mongodb.Run(ctx, "mongo:8", mongodb.WithReplicaSet("rs0"))
	if err != nil {
		return nil, nil, fmt.Errorf("start mongodb: %w", errors.Join(ErrDockerUnavailable, err))
	}
	uri, err := c.ConnectionString(ctx)
	if err != nil {
		_ = c.Terminate(ctx)
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
		_ = c.Terminate(ctx)
		return nil, nil, fmt.Errorf("connect: %w", err)
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
		_ = client.Disconnect(context.Background())
		_ = c.Terminate(ctx)
		return nil, nil, fmt.Errorf("ping mongodb: %w", err)
	}
	inst := &Instance{
		Client:    client,
		DB:        client.Database(dbName),
		terminate: func() { _ = client.Disconnect(context.Background()); _ = c.Terminate(context.Background()) },
	}
	return inst, inst.terminate, nil
}
