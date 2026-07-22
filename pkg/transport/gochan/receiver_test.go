package gochan_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/quarks-tech/protoevent-go/pkg/event"
	"github.com/quarks-tech/protoevent-go/pkg/transport/gochan"
)

// TestReceiveReturnsOnCancelWhileIdle is the regression test for the
// cancellation gap: Receive checked ctx only when a message ARRIVED, so an
// idle subscriber with a canceled ctx blocked until transport Close —
// contradicting Receive's documented contract ("until the channel is closed
// (Close) or ctx is canceled").
func TestReceiveReturnsOnCancelWhileIdle(t *testing.T) {
	tr := gochan.New()
	ctx, cancel := context.WithCancel(t.Context())

	done := make(chan error, 1)
	go func() {
		done <- tr.Receive(ctx, func(*event.Metadata, []byte) error { return nil })
	}()

	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Receive() error = %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Receive did not return after ctx cancellation on an idle channel")
	}
}

// TestReceiveDeliversThenStopsOnClose pins the happy path: buffered events
// reach the processor and Close ends Receive with nil.
func TestReceiveDeliversThenStopsOnClose(t *testing.T) {
	tr := gochan.New()
	ctx := t.Context()

	md := event.NewMetadata("books.created")
	if err := tr.Send(ctx, md, []byte("x")); err != nil {
		t.Fatalf("send: %v", err)
	}
	tr.Close(ctx)

	var got int
	err := tr.Receive(ctx, func(*event.Metadata, []byte) error {
		got++
		return nil
	})
	if err != nil {
		t.Fatalf("Receive() error = %v, want nil after Close", err)
	}
	if got != 1 {
		t.Fatalf("processed = %d, want 1", got)
	}
}
