package eventbus

import (
	"testing"

	"github.com/quarks-tech/protoevent-go/pkg/event"
)

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

			err := s.process(md, []byte("data"))
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

	err := s.process(event.NewMetadata("books.v1.BookCreated"), []byte("data"))
	if !IsUnprocessableEventError(err) {
		t.Fatalf("process() error = %v, want an unprocessable-event error", err)
	}
}
