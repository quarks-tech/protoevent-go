package mongodb_test

// These benchmarks require Docker/testcontainers (see TestMain in
// store_test.go, which they share) and are containerized/opt-in: they only
// run under `go test -bench=...`, never under a plain `go test`.
//
// BenchmarkWatchDeliver (publish N, drain N via the change stream) is
// deliberately NOT implemented here: draining is an unbounded polling loop
// (RunOnce called repeatedly until N events are observed or a deadline —
// see TestStreamRelayEndToEnd in relay_integration_test.go) whose latency is
// dominated by oplog propagation and the DrainWindow poll cadence, not by
// store-side work. A tight, single b.Loop()-iteration timed region would
// either need an artificially large DrainWindow (slow, one full window per
// iteration) or assert exact delivery counts racing the change stream's
// asynchronous batching — flaky under CI/Docker load. A per-Publish and
// per-batch-read number (below) already captures the store-side costs that
// dominate the design's throughput claims.

import (
	"context"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// BenchmarkPublish measures a session+transaction insert per op — the cost a
// business transaction pays to also emit an event.
func BenchmarkPublish(b *testing.B) {
	if testDB == nil {
		b.Skip("no MongoDB")
	}
	b.ReportAllocs()
	reset(b)

	for b.Loop() {
		publish(b, "bench")
	}
}

// BenchmarkWatchDrainRead measures a plain read of 100 pre-inserted envelopes
// via the outbox collection (the storage-side cost independent of the
// change-stream propagation/backoff loop discussed above).
func BenchmarkWatchDrainRead(b *testing.B) {
	if testDB == nil {
		b.Skip("no MongoDB")
	}
	b.ReportAllocs()
	ctx := context.Background()

	reset(b)
	for range 100 {
		publish(b, "s")
	}

	for b.Loop() {
		cur, err := testDB.Collection("outbox").Find(ctx, bson.M{})
		if err != nil {
			b.Fatalf("find: %v", err)
		}
		var docs []struct {
			ID string `bson:"_id"`
		}
		if err := cur.All(ctx, &docs); err != nil {
			b.Fatalf("decode: %v", err)
		}
		if len(docs) != 100 {
			b.Fatalf("read %d docs, want 100", len(docs))
		}
	}
}
