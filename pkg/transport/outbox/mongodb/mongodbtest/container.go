// Package mongodbtest boots an ephemeral single-node MongoDB replica set
// (testcontainers) for integration tests: change streams and transactions both
// require a replica set.
package mongodbtest

import (
	"context"
	"fmt"
	"strings"

	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/testcontainers/testcontainers-go/modules/mongodb"
)

const dbName = "outbox_test"

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
		return nil, nil, fmt.Errorf("start mongodb (Docker unavailable?): %w", err)
	}
	uri, err := c.ConnectionString(ctx)
	if err != nil {
		_ = c.Terminate(ctx)
		return nil, nil, err
	}
	// directConnection: the single-node RS advertises a container-internal
	// address; a direct connection to the mapped port still supports txns and
	// change streams (the server IS a replica-set member).
	dsn := uri + "&directConnection=true"
	if !strings.Contains(uri, "?") {
		dsn = uri + "?directConnection=true"
	}
	client, err := mongo.Connect(options.Client().ApplyURI(dsn)) // v2: no ctx arg
	if err != nil {
		_ = c.Terminate(ctx)
		return nil, nil, err
	}
	inst := &Instance{
		Client:    client,
		DB:        client.Database(dbName),
		terminate: func() { _ = client.Disconnect(context.Background()); _ = c.Terminate(context.Background()) },
	}
	return inst, inst.terminate, nil
}
