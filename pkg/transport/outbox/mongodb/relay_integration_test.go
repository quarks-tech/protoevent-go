package mongodb_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/quarks-tech/protoevent-go/pkg/event"
	mongodbstore "github.com/quarks-tech/protoevent-go/pkg/transport/outbox/mongodb"
	"github.com/quarks-tech/protoevent-go/pkg/transport/outbox/relay/stream"
)

type recordingSender struct {
	mu  sync.Mutex
	ids []string
}

func (r *recordingSender) Send(_ context.Context, md *event.Metadata, _ []byte) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ids = append(r.ids, md.ID)
	return nil
}

func TestStreamRelayEndToEnd(t *testing.T) {
	if testDB == nil {
		t.Skip("no MongoDB")
	}
	reset(t)
	st := mongodbstore.NewRelayStore(testDB)

	sender := &recordingSender{}
	// DrainWindow doubles as the change stream's maxAwaitTime: the relay
	// passes it to Store.Watch (the single latency knob).
	r, err := stream.NewRelay("e2e", st, sender,
		stream.WithDrainWindow(300*time.Millisecond),
		stream.WithLeaseTTL(15*time.Second),
	)
	if err != nil {
		t.Fatalf("new relay: %v", err)
	}

	// Prime the stream (RunOnce opens it at "now"), then publish, then drain.
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := r.RunOnce(ctx); err != nil { // opens the stream, empty first window
		t.Fatalf("prime: %v", err)
	}

	ids := make([]string, 0, 20)
	for range 20 {
		ids = append(ids, publish(t, "s"))
	}

	// Drain windows until all delivered.
	deadline := time.Now().Add(15 * time.Second)
	for {
		if err := r.RunOnce(ctx); err != nil {
			t.Fatalf("run once: %v", err)
		}
		sender.mu.Lock()
		n := len(sender.ids)
		sender.mu.Unlock()
		if n >= 20 || time.Now().After(deadline) {
			break
		}
	}

	sender.mu.Lock()
	defer sender.mu.Unlock()
	if len(sender.ids) != 20 {
		t.Fatalf("delivered %d, want 20", len(sender.ids))
	}
	for i := range ids {
		if sender.ids[i] != ids[i] {
			t.Fatalf("order[%d] = %s, want %s", i, sender.ids[i], ids[i])
		}
	}

	// Offset persisted: a fresh relay resuming from the saved token delivers nothing new.
	tok, _, err := st.LoadToken(context.Background(), "e2e")
	if err != nil || tok == "" {
		t.Fatalf("token not persisted: tok=%q err=%v", tok, err)
	}

	// Prove the resume claim for real, not just that a token was persisted: a
	// SECOND relay instance for the SAME consumer group, over the SAME store,
	// must deliver nothing new on RunOnce. r never called Run, so it never
	// released its leader lock; drop it directly so r2 can acquire
	// leadership (this is a test-only shortcut for what a graceful shutdown's
	// Run-deferred Release would otherwise do).
	if err := testDB.Collection("relay_locks").Drop(context.Background()); err != nil {
		t.Fatalf("drop relay_locks: %v", err)
	}

	sender2 := &recordingSender{}
	r2, err := stream.NewRelay("e2e", st, sender2,
		stream.WithDrainWindow(300*time.Millisecond),
		stream.WithLeaseTTL(15*time.Second),
	)
	if err != nil {
		t.Fatalf("new relay (resume): %v", err)
	}
	if err := r2.RunOnce(ctx); err != nil {
		t.Fatalf("resume run once: %v", err)
	}
	sender2.mu.Lock()
	n2 := len(sender2.ids)
	sender2.mu.Unlock()
	if n2 != 0 {
		t.Fatalf("resumed relay delivered %d new events, want 0 (fresh relay resuming from the saved token must not redeliver)", n2)
	}
}
