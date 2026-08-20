package eventbus

import (
	"context"
	"net/url"
	"strings"
	"testing"
	"time"

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
		"dataschema", "subject", "time",
		"cloudEvents:id", "cloudEvents:source", "cloudEvents:anything",
	}

	for _, name := range reserved {
		t.Run(name, func(t *testing.T) {
			sender := &recordingSender{}
			p := NewPublisher(sender)

			err := publish(t.Context(), "books.v1.BookCreated", &emptypb.Empty{}, p,
				func(md *event.Metadata) { md.Extensions = map[string]any{name: "hijacked"} })
			if err == nil {
				t.Fatalf("publish with extension %q error = nil, want a collision rejection", name)
			}
			if !strings.Contains(err.Error(), "collides") {
				t.Fatalf("publish with extension %q error = %v, want it to name the collision", name, err)
			}
			if sender.sends != 0 {
				t.Fatalf("sender received %d sends, want 0 (the row must never be persisted)", sender.sends)
			}
		})
	}
}

// TestPublishAcceptsOrdinaryExtensions pins that the guard is not over-broad.
//
// The case-varied names matter: the check used to case-FOLD, so an audit or
// tracing extension named "Type"/"Source"/"ID" failed every Publish — over an
// outbox, inside the caller's business transaction. Nothing would have corrupted:
// every serialization here is case-sensitive, so "Source" lands beside "source"
// rather than on top of it, and structured.Marshal's own guard (an exact-match
// lookup) accepts it too.
func TestPublishAcceptsOrdinaryExtensions(t *testing.T) {
	sender := &recordingSender{}
	p := NewPublisher(sender)

	err := publish(t.Context(), "books.v1.BookCreated", &emptypb.Empty{}, p,
		func(md *event.Metadata) {
			md.Extensions = map[string]any{
				"traceparent": "00-abc-01", "tenant": "acme", "cloudevents": "ok",
				"ID": "audit-1", "Source": "audit", "Type": "audit",
			}
		})
	if err != nil {
		t.Fatalf("publish with ordinary extensions: %v", err)
	}
	if sender.sends != 1 {
		t.Fatalf("sender received %d sends, want 1", sender.sends)
	}
}

// TestPublishRejectsUnserializableExtensionValue pins the VALUE half of the
// extension guard, which is the same wedge arriving through the other door.
//
// A nested value is accepted by json.Marshal (structured mode) and rejected by
// amqp091-go's field encoder, which matches only the defined amqp.Table type — so
// a map[string]any extension fails at SEND time, after the outbox row committed
// with the caller's business transaction, as a non-DecodeError no classifier
// claims. The lane then stops on that row every tick forever, recoverable only by
// editing offsets in a live database. The publisher does not choose the content
// mode its consumers use, so the value has to be usable in both.
func TestPublishRejectsUnserializableExtensionValue(t *testing.T) {
	values := map[string]any{
		"map":    map[string]any{"tenant": "acme"},
		"slice":  []string{"a", "b"},
		"struct": struct{ A int }{A: 1},
		"nil":    nil,
		"uint64": uint64(1), // AMQP has no unsigned-64 field type
	}

	for name, value := range values {
		t.Run(name, func(t *testing.T) {
			sender := &recordingSender{}
			p := NewPublisher(sender)

			err := publish(t.Context(), "books.v1.BookCreated", &emptypb.Empty{}, p,
				func(md *event.Metadata) { md.Extensions = map[string]any{"ctx": value} })
			if err == nil {
				t.Fatalf("publish with a %s extension value error = nil, want a rejection", name)
			}
			if sender.sends != 0 {
				t.Fatalf("sender received %d sends, want 0 (the row must never be persisted)", sender.sends)
			}
		})
	}
}

// TestPublishAcceptsSerializableExtensionValues pins that the value guard admits
// every shape both content modes carry — including float64, which is what a
// numeric extension becomes after a JSON round trip through an outbox store.
func TestPublishAcceptsSerializableExtensionValues(t *testing.T) {
	values := map[string]any{
		"string": "acme",
		"bool":   true,
		"int":    42,
		"int64":  int64(42),
		"float":  float64(1.5),
		"bytes":  []byte("raw"),
		"time":   time.Now(),
	}

	for name, value := range values {
		t.Run(name, func(t *testing.T) {
			sender := &recordingSender{}
			p := NewPublisher(sender)

			err := publish(t.Context(), "books.v1.BookCreated", &emptypb.Empty{}, p,
				func(md *event.Metadata) { md.Extensions = map[string]any{"ctx": value} })
			if err != nil {
				t.Fatalf("publish with a %s extension value: %v", name, err)
			}
			if sender.sends != 1 {
				t.Fatalf("sender received %d sends, want 1", sender.sends)
			}
		})
	}
}

// TestPublishNormalizesEmptyDataSchema pins that a non-nil-but-empty DataSchema
// is normalized to nil at publish.
//
// `schema, _ := url.Parse(cfg.SchemaURL)` on an empty config value yields a
// NON-nil &url.URL{} — the standard Go footgun — and WithEventDataSchema takes it
// as given. Carrying it forward makes the event's dataschema differ from the one
// every reader reconstructs, since the decoders map an empty URI back to nil.
func TestPublishNormalizesEmptyDataSchema(t *testing.T) {
	sender := &capturingSender{}
	p := NewPublisher(sender)

	empty, err := url.Parse("")
	if err != nil {
		t.Fatalf("url.Parse(\"\"): %v", err)
	}
	if empty == nil {
		t.Fatal("url.Parse(\"\") returned nil; this test's premise is gone")
	}

	if err := publish(t.Context(), "books.v1.BookCreated", &emptypb.Empty{}, p,
		WithEventDataSchema(empty)); err != nil {
		t.Fatalf("publish: %v", err)
	}
	if sender.md == nil {
		t.Fatal("sender received no event")
	}
	if sender.md.DataSchema != nil {
		t.Fatalf("DataSchema = %v, want nil after normalization", sender.md.DataSchema)
	}
}

type capturingSender struct {
	md *event.Metadata
}

func (s *capturingSender) Send(_ context.Context, md *event.Metadata, _ []byte) error {
	s.md = md

	return nil
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
