package tidb_test

// These benchmarks require Docker/testcontainers (see TestMain in
// store_test.go, which they share) and are containerized/opt-in: they only
// run under `go test -bench=...`, never under a plain `go test`.

import (
	"context"
	"testing"

	tidb "github.com/quarks-tech/protoevent-go/pkg/transport/outbox/tidb"
)

// BenchmarkPublish measures CreateOutboxMessage inside a transaction, per op
// — the cost a business transaction pays to also emit an event.
func BenchmarkPublish(b *testing.B) {
	if testDB == nil {
		b.Skip("no TiDB")
	}
	b.ReportAllocs()
	truncate(b)

	for b.Loop() {
		publish(b, "bench")
	}
}

// BenchmarkSequenceMessages measures the batch-sequence pass (the FOR UPDATE
// + ROW_NUMBER pass) over 500 pre-inserted pending rows. The 500 rows are
// inserted OUTSIDE the timed region each iteration so only the sequencer
// pass itself is measured.
func BenchmarkSequenceMessages(b *testing.B) {
	if testDB == nil {
		b.Skip("no TiDB")
	}
	b.ReportAllocs()
	ctx := context.Background()
	st := tidb.NewRelayStore(testDB)

	for b.Loop() {
		b.StopTimer()
		truncate(b)
		for range 500 {
			publish(b, "s")
		}
		b.StartTimer()

		if _, err := st.SequenceMessages(ctx, 1000); err != nil {
			b.Fatalf("sequence: %v", err)
		}
	}
}

// BenchmarkListMessages measures a drain-read of 100 sequenced messages.
func BenchmarkListMessages(b *testing.B) {
	if testDB == nil {
		b.Skip("no TiDB")
	}
	b.ReportAllocs()
	ctx := context.Background()
	st := tidb.NewRelayStore(testDB)

	truncate(b)
	for range 100 {
		publish(b, "s")
	}
	if _, err := st.SequenceMessages(ctx, 1000); err != nil {
		b.Fatalf("sequence: %v", err)
	}

	for b.Loop() {
		msgs, err := st.ListMessages(ctx, 0, 100)
		if err != nil {
			b.Fatalf("list: %v", err)
		}
		if len(msgs) != 100 {
			b.Fatalf("listed %d, want 100", len(msgs))
		}
	}
}

// BenchmarkPublishParallel measures CreateOutboxMessage inside a transaction
// under concurrent load (b.RunParallel): each goroutine opens its own
// transaction on the pool, publishes one message, and commits, mirroring
// BenchmarkPublish's per-op shape. The design claims the publish path stays
// contention-free (no hot row: no shared counter, no FOR UPDATE) — so this
// parallel ns/op should scale with concurrency, not collapse toward (or
// exceed) BenchmarkPublish's serial ns/op.
func BenchmarkPublishParallel(b *testing.B) {
	if testDB == nil {
		b.Skip("no TiDB")
	}
	b.ReportAllocs()
	truncate(b)

	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			publish(b, "bench-parallel")
		}
	})
}

// BenchmarkCommitOffset measures RelayStore.CommitOffset, the GREATEST
// upsert every relay drain page pays to advance a consumer's watermark. seq
// increments each iteration so every call takes the UPDATE (advancing)
// branch, matching steady-state drain behavior.
func BenchmarkCommitOffset(b *testing.B) {
	if testDB == nil {
		b.Skip("no TiDB")
	}
	b.ReportAllocs()
	truncate(b)
	ctx := context.Background()
	st := tidb.NewRelayStore(testDB)

	var seq int64
	for b.Loop() {
		seq++
		if err := st.CommitOffset(ctx, "bench", seq); err != nil {
			b.Fatalf("commit offset: %v", err)
		}
	}
}
