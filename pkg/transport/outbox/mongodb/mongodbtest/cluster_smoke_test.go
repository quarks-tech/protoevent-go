package mongodbtest_test

import (
	"context"
	"errors"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/quarks-tech/protoevent-go/pkg/transport/outbox/mongodb/mongodbtest"
)

// TestClusterElectsAndFailsOver is the harness's own smoke test: it proves the
// three-node set comes up, that a host-side driver can write to it through the
// replicaSet topology, and that a forced stepdown produces a REAL election the client
// follows — the three things Start's single node cannot do.
func TestClusterElectsAndFailsOver(t *testing.T) {
	cl, cleanup, err := mongodbtest.StartCluster(context.Background())
	if err != nil {
		if errors.Is(err, mongodbtest.ErrDockerUnavailable) {
			t.Skipf("no Docker: %v", err)
		}
		t.Fatalf("StartCluster: %v", err)
	}
	defer cleanup()

	coll := cl.DB.Collection("smoke")

	if _, err := coll.InsertOne(t.Context(), bson.M{"_id": "before"}); err != nil {
		t.Fatalf("insert before stepdown: %v", err)
	}

	// A real election: the primary is severed and a new one has to win.
	cl.StepDownPrimary(t, 10)

	if _, err := coll.InsertOne(t.Context(), bson.M{"_id": "after"}); err != nil {
		t.Fatalf("insert after stepdown (the driver did not follow the new primary): %v", err)
	}

	n, err := coll.CountDocuments(t.Context(), bson.M{})
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 2 {
		t.Fatalf("counted %d documents, want 2: a write was lost across the election", n)
	}

	// Losing one member of three keeps a majority, so writes must still succeed.
	cl.StopMember(t, 2)
	cl.WaitForPrimary(t)

	if _, err := coll.InsertOne(t.Context(), bson.M{"_id": "degraded"}); err != nil {
		t.Fatalf("insert with 2 of 3 members up: %v", err)
	}

	cl.StartMember(t, 2)
}
