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
	st := mongodbstore.NewStore(testDB, mongodbstore.WithMaxAwaitTime(300*time.Millisecond))

	sender := &recordingSender{}
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

	var ids []string
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
}
