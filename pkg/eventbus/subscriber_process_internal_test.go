package eventbus

import (
	"context"
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
