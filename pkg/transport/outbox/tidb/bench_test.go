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
// + ROW_NUMBER pass) over 500 pending rows.
//
// The 500 rows are inserted ONCE, before the loop; each iteration only unsets
// their seq and rewinds the counter so the same pass can run again. That reset
// is two statements, against the ~500 the insert would cost.
//
// The ratio is the point, not the tidiness. b.Loop() sizes the run by the
// TIMED duration, so untimed setup inside the loop is invisible to it and
// inflates wall clock without bound: at 500 untimed round trips per millisecond
// of measured work, `go test -bench=.` with the default 1s benchtime does not
// finish in any useful time. Keeping the reset comparable to the measured pass
// keeps the benchmark's wall clock proportional to its benchtime.
func BenchmarkSequenceMessages(b *testing.B) {
	if testDB == nil {
		b.Skip("no TiDB")
	}
	b.ReportAllocs()
	ctx := context.Background()
	st := tidb.NewRelayStore(testDB)

	truncate(b)
	for range 500 {
		publish(b, "s")
	}

	for b.Loop() {
		b.StopTimer()
		resequence(b)
		b.StartTimer()

		if _, err := st.SequenceMessages(ctx, 1000); err != nil {
			b.Fatalf("sequence: %v", err)
		}
	}
}

// resequence returns every row to the unsequenced state and rewinds the
// sequencer counter, so a sequencer pass can be measured repeatedly over the
// same rows. Deliberately cheap — see BenchmarkSequenceMessages.
func resequence(tb testing.TB) {
	tb.Helper()
	for _, q := range []string{
		"UPDATE outbox_messages SET seq = NULL",
		"UPDATE outbox_sequencers SET next_seq = 1 WHERE name = 'default'",
	} {
		if _, err := testDB.ExecContext(context.Background(), q); err != nil {
			tb.Fatalf("resequence (%s): %v", q, err)
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
// BenchmarkPublish's per-op shape. The design claims the publish path is
// LOCK-contention-free (no shared counter row, no FOR UPDATE — those belong
// to the sequencer pass) — so this parallel ns/op should scale with
// concurrency, not collapse toward (or exceed) BenchmarkPublish's serial
// ns/op. It does NOT claim hotspot-freedom: the AUTO_INCREMENT PK's
// tail-Region write hotspot is the migration header's accepted trade, and a
// single-node benchmark cannot see it either way.
func BenchmarkPublishParallel(b *testing.B) {
	if testDB == nil {
		b.Skip("no TiDB")
	}
	b.ReportAllocs()
	truncate(b)

	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			// Not publish(b, ...): its Fatalf (FailNow) must run on the
			// benchmark's own goroutine, and RunParallel bodies are not it —
			// report with Error and bail out of this body instead.
			if err := publishMetadataErr(newTestMetadata("bench-parallel"), []byte("x")); err != nil {
				b.Errorf("publish: %v", err)
				return
			}
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
