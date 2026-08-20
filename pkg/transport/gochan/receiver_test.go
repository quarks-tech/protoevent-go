package gochan_test

import (
	"context"
	"errors"
	"fmt"
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
		done <- tr.Receive(ctx, func(context.Context, *event.Metadata, []byte) error { return nil })
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
	err := tr.Receive(ctx, func(context.Context, *event.Metadata, []byte) error {
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
		_ = tr.Receive(ctx, func(context.Context, *event.Metadata, []byte) error {
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
	if err := tr.Receive(t.Context(), func(context.Context, *event.Metadata, []byte) error {
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
	err := tr.Receive(ctx, func(context.Context, *event.Metadata, []byte) error {
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

// TestReceiveSurvivesProcessorError pins that a handler failure does not end the
// subscription.
//
// Receive used to return the processor's error, which killed the whole subscriber
// goroutine on the first unhandled event: every message still buffered, and
// everything published afterwards, was silently never processed, while Send kept
// succeeding until the buffer filled and publishers blocked forever. One bad event
// is not a broken transport — and the failures are ordinary (a malformed event
// type, an unknown subscription, a codec failure all reach the processor as
// errors), so the documented contract ("until the channel is closed or ctx is
// canceled") has to hold through them.
func TestReceiveSurvivesProcessorError(t *testing.T) {
	const total = 5

	var failed []error
	tr := gochan.New(gochan.WithErrorHandler(func(_ *event.Metadata, err error) {
		failed = append(failed, err)
	}))

	md := event.NewMetadata("books.created")
	for range total {
		if err := tr.Send(t.Context(), md, []byte("x")); err != nil {
			t.Fatalf("send: %v", err)
		}
	}
	tr.Close(t.Context())

	var seen int
	err := tr.Receive(t.Context(), func(context.Context, *event.Metadata, []byte) error {
		seen++

		return errors.New("handler failed")
	})
	if err != nil {
		t.Fatalf("Receive() error = %v, want nil: a handler failure must not end the subscription", err)
	}
	if seen != total {
		t.Fatalf("delivered = %d, want %d: delivery stopped at the first handler failure", seen, total)
	}
	if len(failed) != total {
		t.Fatalf("error handler saw %d failures, want %d", len(failed), total)
	}
}

// TestSendRacingCloseDoesNotPanic pins that a planned shutdown cannot crash a
// publisher.
//
// Close used to be a bare close() of the shared message channel, so a Send parked
// on a full buffer — the state a slow handler puts every publisher into, and
// exactly what a shutdown interrupts — panicked with "send on closed channel" on a
// goroutine this package does not own. The doc comment waived it as acceptable for
// an in-memory transport while offering callers no barrier with which to satisfy
// the "only after every Send has returned" precondition it required.
func TestSendRacingCloseDoesNotPanic(t *testing.T) {
	sr := gochan.New()

	md := &event.Metadata{ID: "id-1", Type: "books.v1.BookCreated", Source: "/svc"}

	// Fill the buffer so the next Send has nowhere to go.
	for i := range 20 {
		if err := sr.Send(context.Background(), md, []byte("d")); err != nil {
			t.Fatalf("prefill %d: %v", i, err)
		}
	}

	blocked := make(chan error, 1)
	go func() {
		defer func() {
			if p := recover(); p != nil {
				blocked <- fmt.Errorf("Send panicked: %v", p)
			}
		}()
		blocked <- sr.Send(context.Background(), md, []byte("blocked"))
	}()

	// Give the goroutine time to park in the send.
	time.Sleep(50 * time.Millisecond)

	sr.Close(context.Background())

	select {
	case err := <-blocked:
		if !errors.Is(err, gochan.ErrClosed) {
			t.Fatalf("blocked Send returned %v, want gochan.ErrClosed", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("blocked Send never returned after Close")
	}

	// A Send after Close reports it rather than panicking, and Close is idempotent.
	if err := sr.Send(context.Background(), md, []byte("after")); !errors.Is(err, gochan.ErrClosed) {
		t.Fatalf("Send after Close = %v, want gochan.ErrClosed", err)
	}
	sr.Close(context.Background())
}

// TestCloseDeliversWhatWasAlreadyBuffered pins the other half of Close's contract:
// ending the subscription must not discard events the transport already accepted.
func TestCloseDeliversWhatWasAlreadyBuffered(t *testing.T) {
	sr := gochan.New()

	md := &event.Metadata{ID: "id-1", Type: "books.v1.BookCreated", Source: "/svc"}
	for i := range 5 {
		if err := sr.Send(context.Background(), md, []byte{byte(i)}); err != nil {
			t.Fatalf("send %d: %v", i, err)
		}
	}

	sr.Close(context.Background())

	var got int
	err := sr.Receive(context.Background(), func(_ context.Context, _ *event.Metadata, _ []byte) error {
		got++

		return nil
	})
	if err != nil {
		t.Fatalf("Receive = %v, want nil", err)
	}
	if got != 5 {
		t.Fatalf("delivered %d events, want 5", got)
	}
}
