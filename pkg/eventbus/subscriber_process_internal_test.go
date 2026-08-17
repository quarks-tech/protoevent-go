package eventbus

import (
	"context"
	"strings"
	"testing"

	"github.com/quarks-tech/protoevent-go/pkg/event"
)

// TestProcessUsesCallerContext pins that the handler's context is the one the
// TRANSPORT supplied, not a fresh context.Background(). Deriving it from
// Background left handlers with no way to observe shutdown or connection loss at
// all: under a drain-capable receiver a slow handler ran past the drain budget,
// the connection was force-closed under it, and the unacked delivery redelivered
// after restart — running any non-idempotent side effect twice.
func TestProcessUsesCallerContext(t *testing.T) {
	s := NewSubscriber("test")

	type ctxKey struct{}

	sd := &ServiceDesc{ServiceName: "books.v1", Events: []EventDesc{{Name: "BookCreated"}}}

	var got any
	s.RegisterHandler(sd, "BookCreated", func(ctx context.Context, _ *event.Metadata, _ func(any) error, _ SubscriberInterceptor) error {
		got = ctx.Value(ctxKey{})

		return nil
	})

	ctx := context.WithValue(t.Context(), ctxKey{}, "from-transport")

	md := event.NewMetadata("books.v1.BookCreated")
	md.DataContentType = "application/protobuf"

	if err := s.process(ctx, md, []byte("data")); err != nil {
		t.Fatalf("process() error = %v", err)
	}
	if got != "from-transport" {
		t.Fatalf("handler ctx value = %v, want the caller's context to reach the handler", got)
	}
}

// TestProcessRecoversAPanickingHandler pins that a panicking handler cannot take
// the consumer process down.
//
// Nothing in this library recovered a panic: `recover()` appeared only in tests. A
// handler that panics on one malformed-but-well-typed payload therefore killed the
// whole process, and — because the panic unwinds before any Ack/Reject — the
// delivery was never acknowledged, so the broker redelivered it to the replacement
// pod, which died the same way. One bad event became an unbounded crash loop that
// takes every other subscription on the process with it.
//
// This is the same class of failure the malformed-event-type case below was
// hardened against; the difference is that the panic comes from the CALLER's code,
// where the library cannot prevent it and can only contain it. It is reported as an
// unprocessable event because a panic is deterministic in the payload: retrying it
// reproduces it, so the delivery belongs in the dead-letter/parking path, not back
// on the queue.
func TestProcessRecoversAPanickingHandler(t *testing.T) {
	s := NewSubscriber("test")

	sd := &ServiceDesc{ServiceName: "books.v1", Events: []EventDesc{{Name: "BookCreated"}}}
	// The panic VALUE stands in for whatever the handler actually did — a nil map
	// write, an out-of-range index, a nil dereference. Which one is irrelevant to
	// the containment this test is about, and spelling one out literally is the kind
	// of thing static analysis rightly refuses to let into a file.
	s.RegisterHandler(sd, "BookCreated", func(context.Context, *event.Metadata, func(any) error, SubscriberInterceptor) error {
		panic("assignment to entry in nil map")
	})

	md := event.NewMetadata("books.v1.BookCreated")
	md.DataContentType = "application/protobuf"

	err := s.process(t.Context(), md, []byte("data"))
	if err == nil {
		t.Fatal("process() = nil for a panicking handler, want an error")
	}
	if !IsUnprocessableEventError(err) {
		t.Fatalf("process() = %v, want an unprocessable-event error: a panic is deterministic in "+
			"the payload, so requeueing it reproduces the crash forever", err)
	}
	// The operator has to be able to find the handler that did it.
	if !strings.Contains(err.Error(), "panic") {
		t.Fatalf("process() = %v, want the error to say it was a panic", err)
	}
}

// TestProcessRejectsMalformedEventType pins that a malformed incoming event type
// is reported as an unprocessable event, never a panic. md.Type arrives from the
// wire and neither marshaler validates its shape (the binary one only requires a
// non-empty amqp Type, the structured one a non-empty JSON string), so slicing it
// at LastIndex(".") == -1 panicked inside the transport's delivery goroutine.
// Nothing recovers that: the process dies, the delivery is never acked, and the
// redelivery crashes the replacement pod too — the same malformed-metadata class
// event.ContentSubtype is hardened against one line below.
func TestProcessRejectsMalformedEventType(t *testing.T) {
	types := map[string]string{
		"no dot":            "BookCreated",
		"empty":             "",
		"leading dot":       ".BookCreated",
		"trailing dot":      "books.v1.",
		"only a dot":        ".",
		"no service prefix": ".v1",
	}

	for name, eventType := range types {
		t.Run(name, func(t *testing.T) {
			s := NewSubscriber("test")

			md := event.NewMetadata(eventType)
			md.DataContentType = "application/protobuf"

			err := s.process(t.Context(), md, []byte("data"))
			if err == nil {
				t.Fatalf("process(%q) error = nil, want an unprocessable-event error", eventType)
			}
			if !IsUnprocessableEventError(err) {
				t.Fatalf("process(%q) error = %v, want an unprocessable-event error so the transport parks the delivery", eventType, err)
			}
		})
	}
}

// TestProcessRejectsUnknownServiceWithWellFormedType pins that the guard above
// did not swallow the ordinary unknown-subscription path: a well-formed type for
// a service nobody registered is still an unprocessable event.
func TestProcessRejectsUnknownServiceWithWellFormedType(t *testing.T) {
	s := NewSubscriber("test")

	err := s.process(t.Context(), event.NewMetadata("books.v1.BookCreated"), []byte("data"))
	if !IsUnprocessableEventError(err) {
		t.Fatalf("process() error = %v, want an unprocessable-event error", err)
	}
}
