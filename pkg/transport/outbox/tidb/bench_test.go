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
