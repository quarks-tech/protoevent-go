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

// TestReceiveProcessesNothingAfterCancel pins cancellation PRIORITY, which the
// idle-cancellation fix above put at risk: with both the ctx.Done() case and the
// channel case ready, Go picks one pseudo-randomly, so a canceled subscriber
// would keep handling buffered events roughly half the time — running handler
// side effects (writes, outbound calls) after it was told to stop. The buffer is
// filled to the transport's depth so a per-iteration coin flip would almost
// surely deliver at least one event.
//
// Priority is enforced by a pre-dequeue check, not by re-checking after taking a
// message: a message already off the channel cannot be put back, so dropping it
// there would lose it. See TestReceiveDoesNotDropDequeuedMessages.
func TestReceiveProcessesNothingAfterCancel(t *testing.T) {
	tr := gochan.New()
	ctx, cancel := context.WithCancel(t.Context())

	md := event.NewMetadata("books.created")
	for range 20 {
		if err := tr.Send(t.Context(), md, []byte("x")); err != nil {
			t.Fatalf("send: %v", err)
		}
	}

	cancel()

	var processed int
	err := tr.Receive(ctx, func(*event.Metadata, []byte) error {
		processed++

		return nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Receive() error = %v, want context.Canceled", err)
	}
	if processed != 0 {
		t.Fatalf("processed = %d after cancellation, want 0", processed)
	}
}

// TestReceiveDoesNotDropDequeuedMessages pins the other side of the
// cancellation-priority trade-off: every message Receive takes off the channel is
// either processed or still buffered when it returns. A cancellation check placed
// AFTER the dequeue satisfies "nothing processed after cancel" by silently
// discarding the message it just took — unrecoverable, since a receiver cannot
// put one back.
//
// The context is canceled concurrently with delivery, so the cancellation lands
// at an arbitrary point in the drain; whatever the split, processed + still
// buffered must account for every message.
func TestReceiveDoesNotDropDequeuedMessages(t *testing.T) {
	const total = 20

	tr := gochan.New()
	ctx, cancel := context.WithCancel(t.Context())

	md := event.NewMetadata("books.created")
	for range total {
		if err := tr.Send(t.Context(), md, []byte("x")); err != nil {
			t.Fatalf("send: %v", err)
		}
	}

	var processed int
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = tr.Receive(ctx, func(*event.Metadata, []byte) error {
			processed++
			cancel() // cancel mid-drain, racing the next dequeue
			return nil
		})
	}()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Receive did not return after cancellation")
	}

	// Drain what is left with a live context: anything Receive dequeued and
	// dropped would be missing from this total.
	tr.Close(t.Context())

	remaining := 0
	if err := tr.Receive(t.Context(), func(*event.Metadata, []byte) error {
		remaining++
		return nil
	}); err != nil {
		t.Fatalf("drain Receive: %v", err)
	}

	if processed+remaining != total {
		t.Fatalf("processed(%d) + remaining(%d) = %d, want %d: a dequeued message was dropped on cancellation",
			processed, remaining, processed+remaining, total)
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
