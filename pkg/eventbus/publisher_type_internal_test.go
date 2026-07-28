package eventbus

import (
	"context"
	"strings"
	"testing"

	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/quarks-tech/protoevent-go/pkg/event"
)

type recordingSender struct {
	sends int
}

func (s *recordingSender) Send(context.Context, *event.Metadata, []byte) error {
	s.sends++

	return nil
}

// TestPublishRejectsReservedExtensionName pins the WRITE-side collision guard.
//
// The content marshalers reject a colliding extension too, but they run at SEND
// time. Over an outbox that is after the row has committed with the caller's business
// transaction, and a marshal failure is not a DecodeError — so without an
// UnsendableClassifier the relay lane stops on that row every tick forever. Catching
// it at publish keeps the unsendable row from being written.
//
// The reserved set is the UNION over content modes, because the publisher does not
// choose the mode its consumers (or its relay) will use: bare core-attribute names
// collide in structured mode, "cloudEvents:"-prefixed ones in binary mode.
func TestPublishRejectsReservedExtensionName(t *testing.T) {
	reserved := []string{
		"id", "type", "source", "specversion", "data", "datacontenttype",
		"dataschema", "subject", "time", "ID", "Source",
		"cloudEvents:id", "cloudEvents:source", "cloudEvents:anything",
	}

	for _, name := range reserved {
		t.Run(name, func(t *testing.T) {
			sender := &recordingSender{}
			p := NewPublisher(sender)

			err := publish(t.Context(), "books.v1.BookCreated", &struct{}{}, p,
				func(md *event.Metadata) { md.Extensions = map[string]any{name: "hijacked"} })
			if err == nil {
				t.Fatalf("publish with extension %q error = nil, want a collision rejection", name)
			}
			if sender.sends != 0 {
				t.Fatalf("sender received %d sends, want 0 (the row must never be persisted)", sender.sends)
			}
		})
	}
}

// TestPublishAcceptsOrdinaryExtensions pins that the guard is not over-broad.
func TestPublishAcceptsOrdinaryExtensions(t *testing.T) {
	sender := &recordingSender{}
	p := NewPublisher(sender)

	err := publish(t.Context(), "books.v1.BookCreated", &emptypb.Empty{}, p,
		func(md *event.Metadata) {
			md.Extensions = map[string]any{"traceparent": "00-abc-01", "tenant": "acme", "cloudevents": "ok"}
		})
	if err != nil {
		t.Fatalf("publish with ordinary extensions: %v", err)
	}
	if sender.sends != 1 {
		t.Fatalf("sender received %d sends, want 1", sender.sends)
	}
}

// TestPublishRejectsMalformedEventType pins the WRITE-side event-type guard.
//
// Publish is exported and callable with any type string; generated code always
// emits "<service>.<Event>", hand-written calls need not. Rejecting a malformed
// type at publish matters more than it looks: over an outbox the row commits with
// the caller's business transaction, and the relay can then neither send it (the
// RabbitMQ sender needs the dot to split exchange from routing key) nor classify it
// as poison — so the lane stops on that row every tick forever and nothing behind
// it is delivered, recoverable only by editing offsets in a live database.
func TestPublishRejectsMalformedEventType(t *testing.T) {
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
			sender := &recordingSender{}
			p := NewPublisher(sender)

			err := publish(t.Context(), eventType, &struct{}{}, p)
			if err == nil {
				t.Fatalf("publish(%q) error = nil, want a malformed-event-type error", eventType)
			}
			if !strings.Contains(err.Error(), "malformed event type") {
				t.Fatalf("publish(%q) error = %v, want it to name the malformed event type", eventType, err)
			}
			if sender.sends != 0 {
				t.Fatalf("sender received %d sends for a malformed type, want 0 "+
					"(the row must never be persisted: the relay could never send it)", sender.sends)
			}
		})
	}
}
