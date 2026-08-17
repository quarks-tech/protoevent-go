package binary

import (
	"errors"
	"net/url"
	"strings"
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

// TestMarshalRejectsCoreAttributeNameExtension pins the UNION half of the name
// check: a bare "source" is rejected here too, even though binary mode namespaces
// its own core attributes and would survive it.
//
// A publisher does not choose the content mode its consumers use, and over an
// outbox the metadata is persisted and may be relayed by a transport the publisher
// never saw — so a name that is unsafe in EITHER mode has to fail in both, at
// publish time. See event.ReservedExtensionName.
func TestMarshalRejectsCoreAttributeNameExtension(t *testing.T) {
	for _, name := range []string{"source", "id", "type", "data", "subject", "time"} {
		t.Run(name, func(t *testing.T) {
			md := event.NewMetadata("books.v1.BookCreated")
			md.ID = "real-id"
			md.Source = "/svc"
			md.Extensions = map[string]any{name: "hijacked"}

			if _, err := (Marshaler{}).Marshal(md, []byte("x")); err == nil {
				t.Fatalf("Marshal() error = nil for extension %q, want a reserved-name rejection", name)
			}
		})
	}
}

// TestMarshalRejectsUnencodableExtensionValue pins that a value AMQP cannot carry
// fails at MARSHAL time, not at send time.
//
// amqp091-go's field encoder matches only the defined AMQP field types, so a nested
// value fails inside the publish — over an outbox that is after the row committed
// with the caller's business transaction, as a non-DecodeError no classifier claims,
// and the lane then stops on that row every tick forever.
func TestMarshalRejectsUnencodableExtensionValue(t *testing.T) {
	values := map[string]any{
		"map":   map[string]any{"a": 1},
		"slice": []string{"a"},
		"nil":   nil,
	}

	for name, v := range values {
		t.Run(name, func(t *testing.T) {
			md := event.NewMetadata("books.v1.BookCreated")
			md.ID = "real-id"
			md.Source = "/svc"
			md.Extensions = map[string]any{"nested": v}

			if _, err := (Marshaler{}).Marshal(md, []byte("x")); err == nil {
				t.Fatal("Marshal() error = nil, want the unencodable extension value rejected at publish")
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

// TestUnmarshalTreatsEmptyStringOptionalHeadersAsAbsent pins binary mode's
// STRING-valued optional attributes against structured mode's behavior. A
// publisher that emits every header unconditionally is not sending malformed
// input, and for these the empty string genuinely means "not set":
//
//   - an empty dataschema through url.Parse("") yields a NON-NIL &url.URL{}, which
//     event.Metadata.MarshalJSON rejects — so re-publishing the event through an
//     outbox store would fail inside the caller's business transaction.
//
// `time` is deliberately NOT in this set — see the test below.
func TestUnmarshalTreatsEmptyStringOptionalHeadersAsAbsent(t *testing.T) {
	d := &amqp.Delivery{
		Type: "books.v1.BookCreated",
		Headers: amqp.Table{
			"cloudEvents:specversion": "1.0",
			"cloudEvents:id":          "id-1",
			"cloudEvents:source":      "/svc",
			"cloudEvents:subject":     "",
			"cloudEvents:dataschema":  "",
		},
	}

	md, _, err := Marshaler{}.Unmarshal(d)
	if err != nil {
		t.Fatalf("Unmarshal: %v (empty string-valued optional headers must read as absent)", err)
	}
	if md.DataSchema != nil {
		t.Fatalf("DataSchema = %v, want nil: a non-nil empty URL later fails Metadata.MarshalJSON", md.DataSchema)
	}
	if md.Subject != "" {
		t.Fatalf("Subject = %q, want empty", md.Subject)
	}
}

// TestUnmarshalRejectsEmptyTimeHeader pins the ONE optional attribute that does
// not treat present-but-empty as absent, in lockstep with structured mode (whose
// twin test rejects `"time":""`).
//
// The two modes used to disagree here, so the same publisher was accepted over
// one and dead-lettered over the other. Strict is the right side of that
// disagreement: `time` is typed, and mapping an empty value to absent yields a
// zero Metadata.Time — the exact value the outbox read path uses as its poison
// marker. The malformed value would then survive this boundary only to fail
// somewhere worse: inside a caller's business transaction via
// outbox.ValidateMetadata, or as a poison row stopping a relay lane.
func TestUnmarshalRejectsEmptyTimeHeader(t *testing.T) {
	d := &amqp.Delivery{
		Type: "books.v1.BookCreated",
		Headers: amqp.Table{
			"cloudEvents:specversion": "1.0",
			"cloudEvents:id":          "id-1",
			"cloudEvents:source":      "/svc",
			"cloudEvents:time":        "",
		},
	}

	md, _, err := Marshaler{}.Unmarshal(d)
	if err == nil {
		t.Fatalf("Unmarshal accepted an empty 'time' header (Time = %v); want it rejected, "+
			"or a zero Metadata.Time reaches the outbox poison path", md.Time)
	}
	if !strings.Contains(err.Error(), "time") {
		t.Fatalf("error = %v, want it to name the 'time' attribute", err)
	}
}

// TestUnmarshalOmittedTimeHeaderIsAbsent pins that the strictness above is about
// a present-but-empty header, not about `time` being mandatory: CloudEvents makes
// it optional, so an omitted header stays a zero Time without error.
func TestUnmarshalOmittedTimeHeaderIsAbsent(t *testing.T) {
	d := &amqp.Delivery{
		Type: "books.v1.BookCreated",
		Headers: amqp.Table{
			"cloudEvents:specversion": "1.0",
			"cloudEvents:id":          "id-1",
			"cloudEvents:source":      "/svc",
		},
	}

	md, _, err := Marshaler{}.Unmarshal(d)
	if err != nil {
		t.Fatalf("Unmarshal: %v (an omitted 'time' is legal — CloudEvents marks it optional)", err)
	}
	if !md.Time.IsZero() {
		t.Fatalf("Time = %v, want zero", md.Time)
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

// TestUnmarshalAcceptsByteArrayHeaders pins that a core attribute delivered as an
// AMQP byte-array field is read, not reported as missing.
//
// A longstr decodes as string OR []byte depending on the broker and on anything
// between it and this consumer — a shovel, a federation link, a proxy. The bare
// `.(string)` assertion this replaced reported such a header as MISSING, so
// processDelivery wrapped it as an UnprocessableEventError and every delivery from
// that publisher was silently dead-lettered (or parked) with its attributes
// sitting right there, readable. The parking-lot receiver already normalizes
// x-death headers this way for exactly the same reason.
func TestUnmarshalAcceptsByteArrayHeaders(t *testing.T) {
	d := &amqp.Delivery{
		Type: "books.v1.BookCreated",
		Headers: amqp.Table{
			event.BinaryAttrPrefix + "specversion": []byte("1.0"),
			event.BinaryAttrPrefix + "id":          []byte("id-1"),
			event.BinaryAttrPrefix + "source":      []byte("/books"),
			event.BinaryAttrPrefix + "subject":     []byte("book-7"),
			event.BinaryAttrPrefix + "time":        []byte("2026-01-02T15:04:05Z"),
			event.BinaryAttrPrefix + "dataschema":  []byte("https://example.com/s.json"),
		},
	}

	md, _, err := Marshaler{}.Unmarshal(d)
	if err != nil {
		t.Fatalf("Unmarshal() error = %v, want byte-array headers accepted", err)
	}
	if md.SpecVersion != "1.0" || md.ID != "id-1" || md.Source != "/books" {
		t.Fatalf("required attributes = (%q, %q, %q), want (1.0, id-1, /books)", md.SpecVersion, md.ID, md.Source)
	}
	if md.Subject != "book-7" {
		t.Fatalf("Subject = %q, want book-7", md.Subject)
	}
	if md.Time.IsZero() {
		t.Fatal("Time is zero, want the byte-array timestamp parsed")
	}
	if md.DataSchema == nil || md.DataSchema.String() != "https://example.com/s.json" {
		t.Fatalf("DataSchema = %v, want the byte-array URI parsed", md.DataSchema)
	}
}

// TestUnmarshalMarshalIsClosedOverBrokerHeaders pins that what Unmarshal produces,
// Marshal accepts — the property that broke the moment Marshal started REJECTING
// extensions instead of merging them.
//
// The broker writes its own bookkeeping into the same flat header namespace
// extensions live in. `x-death`, present on every delivery that has been
// dead-lettered once, is an []any, so lifting it into Extensions made re-sending
// the Metadata a subscriber just received fail with "value of type
// []interface {} is not a CloudEvents extension value". Over an outbox that is
// fatal rather than noisy: the marshal failure was not a *DecodeError, no
// classifier claimed it, and the lane stopped on that row every tick forever.
func TestUnmarshalMarshalIsClosedOverBrokerHeaders(t *testing.T) {
	d := &amqp.Delivery{
		Type: "books.v1.BookCreated",
		Headers: amqp.Table{
			event.BinaryAttrPrefix + "specversion": "1.0",
			event.BinaryAttrPrefix + "id":          "id-1",
			event.BinaryAttrPrefix + "source":      "/books",
			// Broker bookkeeping, in the extension namespace, of a type no AMQP
			// field table entry can carry back out.
			"x-death":             []any{amqp.Table{"queue": "events", "count": int64(1)}},
			"x-first-death-queue": "events",
			// A genuine publisher extension must survive.
			"tenant": "acme",
		},
	}

	md, _, err := Marshaler{}.Unmarshal(d)
	if err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if got, ok := md.Extensions["tenant"]; !ok || got != "acme" {
		t.Fatalf("Extensions[tenant] = %v (present=%t), want acme: a real extension must not be dropped", got, ok)
	}
	if _, ok := md.Extensions["x-death"]; ok {
		t.Fatal("x-death was lifted into Extensions: broker bookkeeping is not a CloudEvents extension, " +
			"and re-marshaling it fails at send time")
	}

	if _, err := (Marshaler{}).Marshal(md, []byte("x")); err != nil {
		t.Fatalf("Marshal(Unmarshal(d)) = %v, want nil: the round trip must be closed, or a "+
			"once-dead-lettered event can never be re-sent", err)
	}
}

// TestMarshalExtensionRejectionsAreUnsendable pins the marker that lets a relay
// get PAST a persisted row it can never send.
//
// Publish-time validation keeps such metadata out of the store, but an outbox row
// is durable: rows written before those checks existed still reach the sender. An
// unmarked error there is claimed by no classifier, so the lane stops on that row
// every tick forever with nothing behind it delivered. With the marker, a relay
// configured with a PoisonHandler parks it and moves on.
func TestMarshalExtensionRejectionsAreUnsendable(t *testing.T) {
	base := func() *event.Metadata {
		md := event.NewMetadata("books.v1.BookCreated")
		md.ID = "id-1"
		md.Source = "/books"
		return md
	}

	t.Run("reserved name", func(t *testing.T) {
		md := base()
		md.Extensions = map[string]any{"source": "audit"}
		if _, err := (Marshaler{}).Marshal(md, nil); !errors.Is(err, event.ErrUnsendable) {
			t.Fatalf("Marshal error = %v, want it to wrap event.ErrUnsendable", err)
		}
	})

	t.Run("unencodable value", func(t *testing.T) {
		md := base()
		md.Extensions = map[string]any{"ctx": map[string]any{"a": 1}}
		if _, err := (Marshaler{}).Marshal(md, nil); !errors.Is(err, event.ErrUnsendable) {
			t.Fatalf("Marshal error = %v, want it to wrap event.ErrUnsendable", err)
		}
	})
}

// TestUnmarshalDropsBrokerDeathBookkeeping pins that RabbitMQ's dead-lettering
// bookkeeping never becomes part of the event.
//
// The filter here is TYPE-based: `x-death` is dropped only because it is a
// []interface{} that ValidExtensionValue rejects. But the `x-first-death-*` family
// are plain strings, so they sail through the very rule whose comment says these
// "are transport-layer headers that were never part of the event".
//
// Promoting them is not cosmetic. `x-first-death-queue` is what the parking-lot
// receiver's retry budget is keyed on: a forwarder that re-publishes an incoming
// Metadata carries the ORIGINAL service's queue name into a durable outbox row, and
// the downstream receiver then looks for that name in its own x-death entries,
// never finds it, and applies no retry cap at all — the message loops through the
// wait queue forever, re-running side effects, with no log line.
//
// No forgery is needed for this: consuming a once-dead-lettered event and
// re-publishing its Metadata is the documented forward pattern.
func TestUnmarshalDropsBrokerDeathBookkeeping(t *testing.T) {
	d := &amqp.Delivery{
		Type:        "svc.Event",
		ContentType: "application/protobuf",
		Headers: amqp.Table{
			"cloudEvents:id":          "id-1",
			"cloudEvents:source":      "svc",
			"cloudEvents:specversion": "1.0",
			"cloudEvents:time":        "2026-01-01T00:00:00Z",

			// Broker bookkeeping. Every one of these is written by RabbitMQ, not by
			// any publisher.
			"x-death":                []any{amqp.Table{"queue": "q", "count": int64(3)}},
			"x-first-death-queue":    "someone-elses-queue",
			"x-first-death-exchange": "someone-elses.dlx",
			"x-first-death-reason":   "rejected",
			"x-last-death-queue":     "someone-elses-queue",
			"x-last-death-exchange":  "someone-elses.dlx",
			"x-last-death-reason":    "expired",

			// A caller's own extension must be unaffected.
			"tenant": "acme",
		},
	}

	md, _, err := Marshaler{}.Unmarshal(d)
	if err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	for _, name := range []string{
		"x-death",
		"x-first-death-queue", "x-first-death-exchange", "x-first-death-reason",
		"x-last-death-queue", "x-last-death-exchange", "x-last-death-reason",
	} {
		if v, ok := md.Extensions[name]; ok {
			t.Errorf("extension %q = %#v was promoted into the event; broker dead-lettering "+
				"bookkeeping must never become event data (re-publishing it forges another "+
				"service's retry state and unbounds the parking-lot retry cap)", name, v)
		}
	}
	if md.Extensions["tenant"] != "acme" {
		t.Errorf("extension \"tenant\" = %#v, want \"acme\": the filter must not eat a caller's "+
			"own extensions", md.Extensions["tenant"])
	}
}
