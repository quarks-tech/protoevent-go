package structured

import (
	stdjson "encoding/json"
	"errors"
	"testing"
	"time"

	json "github.com/json-iterator/go"
	amqp "github.com/rabbitmq/amqp091-go"

	"github.com/quarks-tech/protoevent-go/pkg/event"
)

// TestMarshalRejectsCoreAttributeCollision pins that an extension cannot overwrite
// a core CloudEvents attribute.
//
// Structured mode puts extensions in the same flat JSON object as the core
// attributes (binary mode does not — it prefixes core attributes with
// cloudEvents:), so merging extensions over the map lets an extension named
// "source", "type" or "data" replace the real attribute. The consumer then routes
// on a type nobody registered and parks every delivery, or decodes the extension
// value as the payload — and the failure only appears after a switch to structured
// mode, with nothing in the error naming the extension.
func TestMarshalRejectsCoreAttributeCollision(t *testing.T) {
	core := []string{"specversion", "id", "type", "source", "data", "datacontenttype", "dataschema", "subject", "time"}

	for _, name := range core {
		t.Run(name, func(t *testing.T) {
			md := event.NewMetadata("books.v1.BookCreated")
			md.ID = "x"
			md.Source = "/svc"
			md.Extensions = map[string]any{name: "hijacked"}

			if _, err := (Marshaler{}).Marshal(md, []byte(`{}`)); err == nil {
				t.Fatalf("Marshal() error = nil for an extension named %q, want a collision error", name)
			}
		})
	}
}

// TestMarshalRejectsBinaryPrefixExtension pins the UNION half of the name check: a
// "cloudEvents:"-prefixed extension is rejected here too, even though this envelope
// puts core attributes in the flat object un-prefixed and would survive it.
//
// A publisher does not choose the content mode its consumers use, and over an
// outbox the metadata is persisted and may be relayed by a transport the publisher
// never saw — so a name that is unsafe in EITHER mode has to fail in both, at
// publish time. See event.ReservedExtensionName.
func TestMarshalRejectsBinaryPrefixExtension(t *testing.T) {
	for _, name := range []string{"cloudEvents:id", "cloudEvents:source", "cloudEvents:specversion"} {
		t.Run(name, func(t *testing.T) {
			md := event.NewMetadata("books.v1.BookCreated")
			md.ID = "x"
			md.Source = "/svc"
			md.Extensions = map[string]any{name: "hijacked"}

			if _, err := (Marshaler{}).Marshal(md, []byte(`{}`)); err == nil {
				t.Fatalf("Marshal() error = nil for an extension named %q, want a reserved-name rejection", name)
			}
		})
	}
}

// TestMarshalAcceptsNonCollidingExtensions pins that ordinary extensions still
// pass through.
func TestMarshalAcceptsNonCollidingExtensions(t *testing.T) {
	md := event.NewMetadata("books.v1.BookCreated")
	md.ID = "x"
	md.Source = "/svc"
	md.Extensions = map[string]any{"traceparent": "00-abc-def-01", "tenant": "acme"}

	pub, err := Marshaler{}.Marshal(md, []byte(`{}`))
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	md2, _, err := Marshaler{}.Unmarshal(&amqp.Delivery{Body: pub.Body})
	if err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if md2.Type != "books.v1.BookCreated" {
		t.Fatalf("Type = %q, want the published type", md2.Type)
	}
	if md2.Extensions["tenant"] != "acme" {
		t.Fatalf("Extensions[tenant] = %v, want acme", md2.Extensions["tenant"])
	}
}

// TestUnmarshalAcceptsScalarAttributes pins wire compatibility for string-valued
// CloudEvents attributes emitted as JSON numbers or booleans.
//
// Every one of these attributes is a string in the spec, but publishers in other
// languages do emit `"id": 12345`, and the quote-trimming decoder this replaced
// accepted them (as "12345"). A strict string-only decoder would make such a
// publisher unconsumable overnight, every delivery failing as unprocessable.
func TestUnmarshalAcceptsScalarAttributes(t *testing.T) {
	bodies := map[string]string{
		"numeric id":        `{"specversion":"1.0","type":"books.v1.BookCreated","id":12345,"source":"/svc","data":{}}`,
		"boolean id":        `{"specversion":"1.0","type":"books.v1.BookCreated","id":true,"source":"/svc","data":{}}`,
		"numeric version":   `{"specversion":1.0,"type":"books.v1.BookCreated","id":"x","source":"/svc","data":{}}`,
		"whitespace-padded": `{"specversion":"1.0","type":"books.v1.BookCreated","id": 12345 ,"source":"/svc","data":{}}`,
	}

	for name, body := range bodies {
		t.Run(name, func(t *testing.T) {
			md, _, err := Marshaler{}.Unmarshal(&amqp.Delivery{Body: []byte(body)})
			if err != nil {
				t.Fatalf("Unmarshal: %v", err)
			}
			if md.Type != "books.v1.BookCreated" {
				t.Fatalf("Type = %q, want books.v1.BookCreated", md.Type)
			}
			if md.ID == "" {
				t.Fatal("ID = \"\", want the scalar coerced to its literal text")
			}
		})
	}
}

// TestUnmarshalRejectsNonScalarAttributes pins the limit of that tolerance: null,
// objects and arrays carry no string value, and the old quote-trimming path
// "accepted" them as their raw JSON text.
func TestUnmarshalRejectsNonScalarAttributes(t *testing.T) {
	bodies := map[string]string{
		"null id":     `{"specversion":"1.0","type":"t.E","id":null,"source":"/svc","data":{}}`,
		"object id":   `{"specversion":"1.0","type":"t.E","id":{"a":1},"source":"/svc","data":{}}`,
		"array id":    `{"specversion":"1.0","type":"t.E","id":[1,2],"source":"/svc","data":{}}`,
		"missing id":  `{"specversion":"1.0","type":"t.E","source":"/svc","data":{}}`,
		"object subj": `{"specversion":"1.0","type":"t.E","id":"x","source":"/svc","subject":{"a":1},"data":{}}`,
	}

	for name, body := range bodies {
		t.Run(name, func(t *testing.T) {
			if _, _, err := (Marshaler{}).Unmarshal(&amqp.Delivery{Body: []byte(body)}); err == nil {
				t.Fatal("Unmarshal() error = nil, want a rejection")
			}
		})
	}
}

// TestUnmarshalAcceptsEmptyRequiredAttributes pins the OTHER side of that limit: a
// present-but-empty required attribute is accepted, matching binary mode.
//
// Rejecting it (which this used to do) dead-lettered 100% of the traffic from any
// publisher that writes every field unconditionally — generated serializers in
// other languages do — from the moment the consumer was upgraded, with nothing
// surfacing on the publisher side. binary/marshaler.go declined the same change
// for the same reason, so rejecting here also made the two content modes disagree
// about identical input.
func TestUnmarshalAcceptsEmptyRequiredAttributes(t *testing.T) {
	body := `{"specversion":"1.0","type":"t.E","id":"x","source":"","data":{}}`

	md, _, err := Marshaler{}.Unmarshal(&amqp.Delivery{Body: []byte(body)})
	if err != nil {
		t.Fatalf("Unmarshal() error = %v, want an empty required attribute accepted", err)
	}
	if md.Source != "" {
		t.Fatalf("Source = %q, want it carried through as empty", md.Source)
	}
	if md.ID != "x" {
		t.Fatalf("ID = %q, want x", md.ID)
	}
}

// TestUnmarshalTreatsNullOptionalAttributesAsAbsent pins that an OPTIONAL attribute
// can never fail a delivery for being absent-shaped. Generated serializers commonly
// write every field, so `"subject": null` is ordinary on the wire; the
// quote-trimming decoder this replaced never failed on it, and routing optional
// attributes through a strict decoder would dead-letter every event from such a
// publisher.
func TestUnmarshalTreatsNullOptionalAttributesAsAbsent(t *testing.T) {
	bodies := map[string]string{
		"null subject":         `{"specversion":"1.0","type":"t.E","id":"x","source":"/svc","subject":null,"data":{}}`,
		"null time":            `{"specversion":"1.0","type":"t.E","id":"x","source":"/svc","time":null,"data":{}}`,
		"null dataschema":      `{"specversion":"1.0","type":"t.E","id":"x","source":"/svc","dataschema":null,"data":{}}`,
		"null datacontenttype": `{"specversion":"1.0","type":"t.E","id":"x","source":"/svc","datacontenttype":null,"data":{}}`,
		"empty subject":        `{"specversion":"1.0","type":"t.E","id":"x","source":"/svc","subject":"","data":{}}`,
		"all of them":          `{"specversion":"1.0","type":"t.E","id":"x","source":"/svc","subject":null,"time":null,"dataschema":null,"datacontenttype":null,"data":{}}`,
	}

	for name, body := range bodies {
		t.Run(name, func(t *testing.T) {
			md, _, err := Marshaler{}.Unmarshal(&amqp.Delivery{Body: []byte(body)})
			if err != nil {
				t.Fatalf("Unmarshal: %v (a null optional attribute must read as absent, not malformed)", err)
			}
			if md.Subject != "" {
				t.Fatalf("Subject = %q, want empty", md.Subject)
			}
			if !md.Time.IsZero() {
				t.Fatalf("Time = %v, want zero", md.Time)
			}
			if md.DataSchema != nil {
				t.Fatalf("DataSchema = %v, want nil", md.DataSchema)
			}
		})
	}
}

// TestUnmarshalRejectsNonScalarOptionalAttributes pins the limit: an object or
// array carries no string to use, and accepting its raw JSON text is what produced
// garbage metadata before.
func TestUnmarshalRejectsNonScalarOptionalAttributes(t *testing.T) {
	bodies := map[string]string{
		"object subject": `{"specversion":"1.0","type":"t.E","id":"x","source":"/svc","subject":{"a":1},"data":{}}`,
		"array subject":  `{"specversion":"1.0","type":"t.E","id":"x","source":"/svc","subject":[1,2],"data":{}}`,
		"object time":    `{"specversion":"1.0","type":"t.E","id":"x","source":"/svc","time":{"a":1},"data":{}}`,
		// An explicitly-present empty time is a malformed timestamp, not "absent":
		// accepting it yields a zero Metadata.Time, the outbox poison marker.
		"empty time":   `{"specversion":"1.0","type":"t.E","id":"x","source":"/svc","time":"","data":{}}`,
		"garbage time": `{"specversion":"1.0","type":"t.E","id":"x","source":"/svc","time":"not-a-date","data":{}}`,
	}

	for name, body := range bodies {
		t.Run(name, func(t *testing.T) {
			if _, _, err := (Marshaler{}).Unmarshal(&amqp.Delivery{Body: []byte(body)}); err == nil {
				t.Fatal("Unmarshal() error = nil, want a rejection")
			}
		})
	}
}

// TestUnmarshalInterpretsEscapesInOptionalAttributes pins that optional
// attributes go through the same JSON decoder as the required ones. The
// quote-trimming path left escape sequences uninterpreted, so a subject
// containing a quote arrived corrupted.
func TestUnmarshalInterpretsEscapesInOptionalAttributes(t *testing.T) {
	body := `{"specversion":"1.0","type":"t.E","id":"x","source":"/svc","subject":"a\"b","data":{}}`

	md, _, err := Marshaler{}.Unmarshal(&amqp.Delivery{Body: []byte(body)})
	if err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if md.Subject != `a"b` {
		t.Fatalf("Subject = %q, want %q (escapes must be interpreted, not left raw)", md.Subject, `a"b`)
	}
}

// TestMarshalRejectsPayloadThatIsNotJSON pins that structured mode never emits an
// invalid envelope.
//
// The payload is spliced in as json.RawMessage, which the encoder copies through
// verbatim, so before this check a non-JSON payload produced an invalid document
// with a NIL error: proto bytes emitted `"data":\b\x01` and a zero-length non-nil
// payload emitted the literal `"data":`. The publish succeeded, the broker
// accepted the frame, and every consumer failed at json.Unmarshal — so the event
// was dead-lettered while the relay committed its offset past it.
//
// The rejection wraps event.ErrUnsendable so a relay holding such a persisted row
// can park it instead of stopping the lane on it forever.
func TestMarshalRejectsPayloadThatIsNotJSON(t *testing.T) {
	for _, tc := range []struct {
		name string
		data []byte
	}{
		{"proto bytes", []byte("\x08\x01")},
		{"truncated object", []byte("{")},
		{"bare word", []byte("garbage")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			md := event.NewMetadata("books.v1.BookCreated")
			md.ID = "id-1"
			md.Source = "/svc"

			_, err := Marshaler{}.Marshal(md, tc.data)
			if err == nil {
				t.Fatal("Marshal returned nil error for a non-JSON payload")
			}
			if !errors.Is(err, event.ErrUnsendable) {
				t.Fatalf("error does not wrap event.ErrUnsendable, so a relay cannot park the row: %v", err)
			}
		})
	}
}

// TestMarshalEncodesAbsentPayloadAsNull pins that an event with no payload
// round-trips as a VALID envelope, whether the payload arrives as nil or as the
// []byte{} a store normalizes nil to. These two must encode identically: the same
// event published directly and published through an outbox must not differ.
func TestMarshalEncodesAbsentPayloadAsNull(t *testing.T) {
	for _, tc := range []struct {
		name string
		data []byte
	}{
		{"nil", nil},
		{"empty non-nil", []byte{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			md := event.NewMetadata("books.v1.BookDeleted")
			md.ID = "id-1"
			md.Source = "/svc"

			pub, err := Marshaler{}.Marshal(md, tc.data)
			if err != nil {
				t.Fatalf("Marshal: %v", err)
			}
			if !json.Valid(pub.Body) {
				t.Fatalf("emitted an invalid JSON envelope: %q", pub.Body)
			}

			// Decoded with encoding/json, not the json-iterator alias this file
			// imports: jsoniter decodes a JSON null into an EMPTY RawMessage, so it
			// cannot distinguish "data":null from a missing key.
			var dto map[string]stdjson.RawMessage
			if err := stdjson.Unmarshal(pub.Body, &dto); err != nil {
				t.Fatalf("unmarshal envelope: %v", err)
			}
			if got := string(dto["data"]); got != "null" {
				t.Fatalf(`data = %s, want null`, got)
			}
		})
	}
}

// TestRoundTripPreservesSubSecondTime pins that structured mode carries the
// fractional seconds the publisher stamped. Both marshalers formatted with
// time.RFC3339, which has no fractional-seconds field, so every timestamp was
// silently truncated to a whole second — while the outbox persisted the full
// value, making the durable record and the delivered event disagree.
func TestRoundTripPreservesSubSecondTime(t *testing.T) {
	md := event.NewMetadata("books.v1.BookCreated")
	md.ID = "id-1"
	md.Source = "/svc"
	md.Time = time.Date(2026, 8, 18, 12, 0, 0, 123456789, time.UTC)

	pub, err := Marshaler{}.Marshal(md, []byte("{}"))
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	got, _, err := Marshaler{}.Unmarshal(&amqp.Delivery{
		ContentType: pub.ContentType, Body: pub.Body,
	})
	if err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if !got.Time.Equal(md.Time) {
		t.Fatalf("Time = %v, want %v", got.Time, md.Time)
	}
	if got.Time.Nanosecond() != md.Time.Nanosecond() {
		t.Fatalf("Time nanoseconds = %d, want %d (sub-second precision lost on the wire)",
			got.Time.Nanosecond(), md.Time.Nanosecond())
	}
}
