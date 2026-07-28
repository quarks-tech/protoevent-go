package binary

import (
	"net/url"
	"testing"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"

	"github.com/quarks-tech/protoevent-go/pkg/event"
)

// TestMarshalRejectsReservedPrefixExtension pins that an extension cannot overwrite
// a core attribute in BINARY mode — the default content mode, and the one the
// structured-mode collision guard originally missed.
//
// Binary mode namespaces core attributes under "cloudEvents:" and writes extensions
// un-prefixed, so copying extensions over the header table lets
// WithEventExtension("cloudEvents:id", …) replace the attribute the consumer reads
// its Metadata.ID — its dedup key — out of.
func TestMarshalRejectsReservedPrefixExtension(t *testing.T) {
	for _, name := range []string{
		"cloudEvents:id", "cloudEvents:source", "cloudEvents:specversion",
		"cloudEvents:time", "cloudEvents:subject", "cloudEvents:dataschema",
	} {
		t.Run(name, func(t *testing.T) {
			md := event.NewMetadata("books.v1.BookCreated")
			md.ID = "real-id"
			md.Source = "/svc"
			md.Extensions = map[string]any{name: "hijacked"}

			if _, err := (Marshaler{}).Marshal(md, []byte("x")); err == nil {
				t.Fatalf("Marshal() error = nil for extension %q, want a reserved-prefix rejection", name)
			}
		})
	}
}

// TestMarshalAcceptsOrdinaryExtensions pins that un-prefixed extensions still pass
// through and survive a round trip.
func TestMarshalAcceptsOrdinaryExtensions(t *testing.T) {
	md := event.NewMetadata("books.v1.BookCreated")
	md.ID = "id-1"
	md.Source = "/svc"
	md.Extensions = map[string]any{"traceparent": "00-abc-01", "tenant": "acme"}

	pub, err := Marshaler{}.Marshal(md, []byte("x"))
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	got, _, err := Marshaler{}.Unmarshal(&amqp.Delivery{
		Type: md.Type, ContentType: md.DataContentType, Headers: pub.Headers, Body: pub.Body,
	})
	if err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if got.ID != "id-1" {
		t.Fatalf("ID = %q, want id-1 (a core attribute must survive extensions)", got.ID)
	}
	if got.Extensions["tenant"] != "acme" {
		t.Fatalf("Extensions[tenant] = %v, want acme", got.Extensions["tenant"])
	}
}

// TestUnmarshalTreatsEmptyOptionalHeadersAsAbsent pins binary mode's optional
// attributes against structured mode's behavior. A publisher that emits every
// header unconditionally is not sending malformed input, and the two modes must not
// disagree about it:
//
//   - an empty dataschema through url.Parse("") yields a NON-NIL &url.URL{}, which
//     event.Metadata.MarshalJSON rejects — so re-publishing the event through an
//     outbox store would fail inside the caller's business transaction;
//   - an empty time would fail time.Parse and dead-letter the whole delivery.
func TestUnmarshalTreatsEmptyOptionalHeadersAsAbsent(t *testing.T) {
	d := &amqp.Delivery{
		Type: "books.v1.BookCreated",
		Headers: amqp.Table{
			"cloudEvents:specversion": "1.0",
			"cloudEvents:id":          "id-1",
			"cloudEvents:source":      "/svc",
			"cloudEvents:subject":     "",
			"cloudEvents:time":        "",
			"cloudEvents:dataschema":  "",
		},
	}

	md, _, err := Marshaler{}.Unmarshal(d)
	if err != nil {
		t.Fatalf("Unmarshal: %v (empty optional headers must read as absent)", err)
	}
	if md.DataSchema != nil {
		t.Fatalf("DataSchema = %v, want nil: a non-nil empty URL later fails Metadata.MarshalJSON", md.DataSchema)
	}
	if !md.Time.IsZero() {
		t.Fatalf("Time = %v, want zero", md.Time)
	}
	if md.Subject != "" {
		t.Fatalf("Subject = %q, want empty", md.Subject)
	}
}

// TestUnmarshalAcceptsEmptyRequiredHeader pins that a present-but-empty REQUIRED
// header is not a rejection. The per-header blocks this code replaced accepted it, so
// rejecting it would dead-letter every delivery from a publisher that worked before
// the upgrade.
func TestUnmarshalAcceptsEmptyRequiredHeader(t *testing.T) {
	d := &amqp.Delivery{
		Type: "books.v1.BookCreated",
		Headers: amqp.Table{
			"cloudEvents:specversion": "1.0",
			"cloudEvents:id":          "id-1",
			"cloudEvents:source":      "",
		},
	}

	md, _, err := Marshaler{}.Unmarshal(d)
	if err != nil {
		t.Fatalf("Unmarshal: %v (an empty required header must not dead-letter the delivery)", err)
	}
	if md.Source != "" {
		t.Fatalf("Source = %q, want empty", md.Source)
	}
}

// TestUnmarshalRejectsMissingRequiredHeader pins that genuinely ABSENT is still an
// error — the distinction the previous test depends on.
func TestUnmarshalRejectsMissingRequiredHeader(t *testing.T) {
	d := &amqp.Delivery{
		Type: "books.v1.BookCreated",
		Headers: amqp.Table{
			"cloudEvents:specversion": "1.0",
			"cloudEvents:id":          "id-1",
		},
	}

	if _, _, err := (Marshaler{}).Unmarshal(d); err == nil {
		t.Fatal("Unmarshal() error = nil for a missing required header, want a rejection")
	}
}

// TestRoundTripPreservesDataSchemaAndTime guards the happy path the empty-header
// handling sits next to.
func TestRoundTripPreservesDataSchemaAndTime(t *testing.T) {
	u, err := url.Parse("https://example.com/s.json")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	md := event.NewMetadata("books.v1.BookCreated")
	md.ID = "id-1"
	md.Source = "/svc"
	md.DataSchema = u
	md.Time = time.Now().UTC().Truncate(time.Second)

	pub, err := Marshaler{}.Marshal(md, []byte("x"))
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	got, _, err := Marshaler{}.Unmarshal(&amqp.Delivery{
		Type: md.Type, Headers: pub.Headers, Body: pub.Body,
	})
	if err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if got.DataSchema == nil || got.DataSchema.String() != u.String() {
		t.Fatalf("DataSchema = %v, want %v", got.DataSchema, u)
	}
	if !got.Time.Equal(md.Time) {
		t.Fatalf("Time = %v, want %v", got.Time, md.Time)
	}
}
