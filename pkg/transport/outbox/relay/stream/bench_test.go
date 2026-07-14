package stream_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/quarks-tech/protoevent-go/pkg/event"
	"github.com/quarks-tech/protoevent-go/pkg/transport/outbox/relay/stream"
)

// BenchmarkDrainWindow measures a single RunOnce draining a fake stream
// pre-loaded with TokenBatchSize events to a no-op sender. The fake
// stream/store are rebuilt per iteration OUTSIDE the timed section
// (b.StopTimer/b.StartTimer) so only the RunOnce call itself is measured.
func BenchmarkDrainWindow(b *testing.B) {
	const n = 100 // matches the default TokenBatchSize

	sender := senderFunc(func(context.Context, *event.Metadata, []byte) error { return nil })

	b.ReportAllocs()
	for b.Loop() {
		b.StopTimer()
		events := make([]*stream.Event, n)
		for i := range events {
			events[i] = ev(fmt.Sprintf("tok-%d", i))
		}
		st := &fakeStore{stream: &fakeStream{events: events}}
		r, err := stream.NewRelay("bench", st, sender)
		if err != nil {
			b.Fatalf("NewRelay: %v", err)
		}
		b.StartTimer()

		if err := r.RunOnce(context.Background()); err != nil {
			b.Fatalf("RunOnce: %v", err)
		}
	}
}
