package sequence_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/quarks-tech/protoevent-go/pkg/event"
	"github.com/quarks-tech/protoevent-go/pkg/transport/outbox/relay/sequence"
)

// BenchmarkDrain measures a single RunOnce fully sequencing and draining N
// pre-pending messages to a no-op sender, table-driven over N and
// SequenceBatchSize. The fake store is rebuilt per iteration OUTSIDE the
// timed section (b.StopTimer/b.StartTimer) so only the RunOnce call itself is
// measured, not per-iteration setup.
func BenchmarkDrain(b *testing.B) {
	cases := []struct {
		n     int
		batch int
	}{
		{n: 100, batch: 100},
		{n: 100, batch: 1000},
		{n: 1000, batch: 100},
		{n: 1000, batch: 1000},
	}

	sender := senderFunc(func(context.Context, *event.Metadata, []byte) error { return nil })

	for _, tc := range cases {
		b.Run(fmt.Sprintf("N=%d/SequenceBatchSize=%d", tc.n, tc.batch), func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				b.StopTimer()
				st := newFakeStore()
				for range tc.n {
					st.append(msg(0))
				}
				r, err := sequence.NewRelay("bench", st, sender,
					sequence.WithSequenceBatchSize(tc.batch), sequence.WithStartFromBeginning())
				if err != nil {
					b.Fatalf("NewRelay: %v", err)
				}
				b.StartTimer()

				if err := r.RunOnce(context.Background()); err != nil {
					b.Fatalf("RunOnce: %v", err)
				}
			}
		})
	}
}
