package event_test

import (
	"encoding/json"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/quarks-tech/protoevent-go/pkg/event"
)

// TestMetadataDataSchemaSurvivesJSONRoundTrip pins that DataSchema comes back as
// the URI that was published.
//
// A *url.URL marshals as a struct, and its User *url.Userinfo field — which has
// only unexported fields — marshals to `{}` and unmarshals back to a NON-NIL empty
// Userinfo. URL.String() then emits a spurious "//@" authority, so the consumer
// reads a different schema URI than the publisher sent. The outbox stores persist
// Metadata with json.Marshal, so the corruption is durable: it is written into the
// row and relayed from there.
func TestMetadataDataSchemaSurvivesJSONRoundTrip(t *testing.T) {
	schemas := []string{
		"https://example.com/schemas/book.json",
		"https://example.com/schemas/book.json?v=2#frag",
		"/relative/schema.json",
		"urn:example:schema:book:1",
		"https://user:pass@example.com/schema.json",
	}

	for _, raw := range schemas {
		t.Run(raw, func(t *testing.T) {
			u, err := url.Parse(raw)
			if err != nil {
				t.Fatalf("parse fixture: %v", err)
			}

			md := event.NewMetadata("books.v1.BookCreated")
			md.ID = "id-1"
			md.Source = "/svc"
			md.Time = time.Now().UTC().Truncate(time.Second)
			md.DataSchema = u

			b, err := json.Marshal(md)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}

			var got event.Metadata
			if err := json.Unmarshal(b, &got); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}

			if got.DataSchema == nil {
				t.Fatal("DataSchema = nil after the round trip")
			}
			if got.DataSchema.String() != raw {
				t.Fatalf("DataSchema = %q after the round trip, want %q", got.DataSchema.String(), raw)
			}
		})
	}
}

// TestMetadataRoundTripPreservesEveryField guards the hand-written marshaler
// against a field being added to Metadata and silently dropped on the wire.
func TestMetadataRoundTripPreservesEveryField(t *testing.T) {
	u, err := url.Parse("https://example.com/s.json")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	md := &event.Metadata{
		SpecVersion:     "1.0",
		Type:            "books.v1.BookCreated",
		Source:          "/svc",
		Subject:         "book/7",
		ID:              "id-1",
		Time:            time.Now().UTC().Truncate(time.Second),
		Extensions:      map[string]any{"tenant": "acme"},
		DataSchema:      u,
		DataContentType: "application/protobuf",
	}

	b, err := json.Marshal(md)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var got event.Metadata
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	switch {
	case got.SpecVersion != md.SpecVersion:
		t.Errorf("SpecVersion = %q, want %q", got.SpecVersion, md.SpecVersion)
	case got.Type != md.Type:
		t.Errorf("Type = %q, want %q", got.Type, md.Type)
	case got.Source != md.Source:
		t.Errorf("Source = %q, want %q", got.Source, md.Source)
	case got.Subject != md.Subject:
		t.Errorf("Subject = %q, want %q", got.Subject, md.Subject)
	case got.ID != md.ID:
		t.Errorf("ID = %q, want %q", got.ID, md.ID)
	case !got.Time.Equal(md.Time):
		t.Errorf("Time = %v, want %v", got.Time, md.Time)
	case got.DataContentType != md.DataContentType:
		t.Errorf("DataContentType = %q, want %q", got.DataContentType, md.DataContentType)
	case got.Extensions["tenant"] != "acme":
		t.Errorf("Extensions[tenant] = %v, want acme", got.Extensions["tenant"])
	case got.DataSchema == nil || got.DataSchema.String() != u.String():
		t.Errorf("DataSchema = %v, want %v", got.DataSchema, u)
	}
}

// TestMetadataDecodesLegacyDataSchemaShape pins backward compatibility for rows
// already in a store.
//
// Before Metadata had a custom marshaler, DataSchema was persisted as the
// marshaled url.URL STRUCT. Outbox rows are durable, so those rows still have to
// decode: rejecting the old shape would classify every one of them as a poison row
// and stop the relay lane at the first one — turning a fidelity fix into an
// outage.
func TestMetadataDecodesLegacyDataSchemaShape(t *testing.T) {
	// Exactly what json.Marshal(*url.URL) produced, empty Userinfo included.
	legacy := `{"SpecVersion":"1.0","Type":"books.v1.BookCreated","Source":"/svc","Subject":"","ID":"id-1",` +
		`"Time":"2026-07-27T10:00:00Z","Extensions":null,` +
		`"DataSchema":{"Scheme":"https","Opaque":"","User":{},"Host":"example.com","Path":"/schemas/book.json",` +
		`"RawPath":"","OmitHost":false,"ForceQuery":false,"RawQuery":"","Fragment":"","RawFragment":""},` +
		`"DataContentType":"application/protobuf"}`

	var md event.Metadata
	if err := json.Unmarshal([]byte(legacy), &md); err != nil {
		t.Fatalf("unmarshal legacy row: %v (an existing row must not become poison)", err)
	}

	if md.Type != "books.v1.BookCreated" {
		t.Fatalf("Type = %q, want books.v1.BookCreated", md.Type)
	}
	if md.DataSchema == nil {
		t.Fatal("DataSchema = nil for a legacy row that had one")
	}
	// The empty Userinfo must be dropped: leaving it non-nil is what emitted the
	// spurious "//@" authority.
	if got := md.DataSchema.String(); got != "https://example.com/schemas/book.json" {
		t.Fatalf("DataSchema = %q, want https://example.com/schemas/book.json", got)
	}

	// Re-marshaling upgrades the row to the string shape.
	b, err := json.Marshal(&md)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(b), `"DataSchema":"https://example.com/schemas/book.json"`) {
		t.Fatalf("re-marshaled DataSchema is not the URI string form: %s", b)
	}
}

// TestMetadataOmitsEmptyDataSchema pins that a non-nil-but-empty DataSchema is
// OMITTED, not rejected.
//
// It used to be an error, and the cost landed nowhere near the mistake:
// `schema, _ := url.Parse(cfg.SchemaURL)` on an empty config value yields a
// non-nil &url.URL{}, and the outbox stores marshal metadata INSIDE the caller's
// business transaction — so a benign misconfiguration rolled back every request,
// on the outbox path only, while the identical event published fine over
// RabbitMQ. Omitting writes exactly what a read reconstructs (the decoder maps
// both absent and "" to nil), so the round trip stays honest.
func TestMetadataOmitsEmptyDataSchema(t *testing.T) {
	md := event.NewMetadata("books.v1.BookCreated")
	md.ID = "id-1"
	md.DataSchema = &url.URL{}

	b, err := json.Marshal(md)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(b), `"DataSchema"`) {
		t.Fatalf("empty DataSchema was encoded, want it omitted: %s", b)
	}

	var got event.Metadata
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.DataSchema != nil {
		t.Fatalf("DataSchema = %v, want nil after the round trip", got.DataSchema)
	}
}

// TestMetadataNilDataSchemaStaysNil pins that an absent dataschema does not
// materialize as a non-nil empty URL.
func TestMetadataNilDataSchemaStaysNil(t *testing.T) {
	md := event.NewMetadata("books.v1.BookCreated")
	md.ID = "id-1"

	b, err := json.Marshal(md)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var got event.Metadata
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.DataSchema != nil {
		t.Fatalf("DataSchema = %v, want nil", got.DataSchema)
	}
}
