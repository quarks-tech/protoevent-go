// Package event holds the CloudEvents 1.0 metadata model shared by every
// transport: the Metadata envelope, its context plumbing for subscriber
// handlers, and content-type parsing for codec selection.
package event

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/url"
	"strings"
	"time"
)

// Metadata is the CloudEvents 1.0 event envelope
// (https://github.com/cloudevents/spec). Publishers fill Type/Source/etc. via
// eventbus publish options; the bus completes ID and Time when the caller
// leaves them zero. Extensions values round-trip through JSON in some stores,
// so numeric extension values come back as float64.
type Metadata struct {
	SpecVersion     string
	Type            string
	Source          string
	Subject         string
	ID              string
	Time            time.Time
	Extensions      map[string]any
	DataSchema      *url.URL
	DataContentType string
}

// metadataJSON is Metadata's wire shape. It exists only so DataSchema crosses
// JSON as its URI text.
//
// A *url.URL does NOT survive a JSON round trip as a struct: its unexported-field
// User *url.Userinfo marshals to `{}` and unmarshals back to a NON-NIL empty
// Userinfo, so URL.String() then emits a spurious "//@" authority and the schema
// URI a consumer reads differs from the one that was published. The outbox stores
// persist Metadata with json.Marshal, so without this the corruption is durable.
// CloudEvents defines dataschema as a URI-reference string anyway, which is what
// this writes.
//
// Field names match Metadata's exported names exactly, so the envelope keys are
// unchanged from the plain-struct marshaling this replaces.
//
// The shape is split in two because the read and write sides are not symmetric.
// Writing always emits the URI as a plain string (metadataJSONOut). Reading has to
// accept the legacy url.URL struct as well — rows persisted before this marshaler
// existed hold it, and rejecting those would turn every one into a poison row and
// stop the relay lane at it — so the decode shape takes a json.RawMessage and
// decodeDataSchema sorts out which it is.
type metadataJSON struct {
	SpecVersion     string          `json:"SpecVersion"`
	Type            string          `json:"Type"`
	Source          string          `json:"Source"`
	Subject         string          `json:"Subject"`
	ID              string          `json:"ID"`
	Time            time.Time       `json:"Time"`
	Extensions      map[string]any  `json:"Extensions"`
	DataSchema      json.RawMessage `json:"DataSchema,omitempty"`
	DataContentType string          `json:"DataContentType"`
}

// metadataJSONOut is the write shape: identical keys and order, with DataSchema as
// the string it always is on the way out. Encoding it directly saves marshaling the
// URI into a json.RawMessage that the outer encoder then re-scans — per persisted
// event, inside the caller's business transaction.
type metadataJSONOut struct {
	SpecVersion     string         `json:"SpecVersion"`
	Type            string         `json:"Type"`
	Source          string         `json:"Source"`
	Subject         string         `json:"Subject"`
	ID              string         `json:"ID"`
	Time            time.Time      `json:"Time"`
	Extensions      map[string]any `json:"Extensions"`
	DataSchema      string         `json:"DataSchema,omitempty"`
	DataContentType string         `json:"DataContentType"`
}

// MarshalJSON implements json.Marshaler. See metadataJSON.
func (m Metadata) MarshalJSON() ([]byte, error) {
	out := metadataJSONOut{
		SpecVersion:     m.SpecVersion,
		Type:            m.Type,
		Source:          m.Source,
		Subject:         m.Subject,
		ID:              m.ID,
		Time:            m.Time,
		Extensions:      m.Extensions,
		DataContentType: m.DataContentType,
	}
	if m.DataSchema != nil {
		// An outbox row is durable, so anything written here must be readable back.
		// A value that is not is worse than a failed publish — it is a row that
		// decodes to something different, or not at all, on every future read.
		uri := m.DataSchema.String()
		switch uri {
		case "":
			// OMITTED, not rejected. An empty URI carries no dataschema, and the
			// decoder already maps both an absent attribute and an empty string to a
			// nil DataSchema — so omitting it writes exactly what every future read
			// reconstructs, which is all this marshaler owes the row.
			//
			// Rejecting is what this used to do, and the cost was out of all
			// proportion to the mistake: `schema, _ := url.Parse(cfg.SchemaURL)` on an
			// empty config value yields a NON-nil &url.URL{} (the standard Go
			// footgun), and the store marshals metadata INSIDE the caller's business
			// transaction — so a benign misconfiguration rolled back every request,
			// on the outbox path only, while the identical event published fine over
			// RabbitMQ. Publishers normalize this to nil up front (see
			// eventbus.publish); this is the durable-path backstop.
		default:
			// url.URL.String() does not guarantee output that url.Parse accepts, nor
			// output that is already canonical: a URL assembled field-by-field (or
			// reconstructed from a legacy persisted row) can hold {Scheme: "0"},
			// which renders as "0:" and Parse rejects; {Scheme: "A"} renders as "A:"
			// but Parse normalizes the scheme, so a re-read then re-write would
			// produce "a:" and the stored bytes would change under a value that
			// never did.
			//
			// So parse, and persist what the parse yields — the exact form every
			// future read will reconstruct. That makes writing idempotent by
			// construction and keeps an unreadable row from ever being stored, which
			// matters because the relay would classify such a row as poison and stop
			// the lane at it. Both cases found by FuzzMetadataJSONRoundTrip.
			parsed, err := url.Parse(uri)
			if err != nil {
				return nil, fmt.Errorf("event: DataSchema %q does not round-trip through url.Parse "+
					"(a persisted row would be unreadable): %w", uri, err)
			}
			uri = parsed.String()
		}

		out.DataSchema = uri
	}

	b, err := json.Marshal(out)
	if err != nil {
		return nil, fmt.Errorf("event: marshal metadata: %w", err)
	}

	return b, nil
}

// UnmarshalJSON implements json.Unmarshaler. See metadataJSON.
func (m *Metadata) UnmarshalJSON(b []byte) error {
	var in metadataJSON
	if err := json.Unmarshal(b, &in); err != nil {
		return fmt.Errorf("event: unmarshal metadata: %w", err)
	}

	*m = Metadata{
		SpecVersion:     in.SpecVersion,
		Type:            in.Type,
		Source:          in.Source,
		Subject:         in.Subject,
		ID:              in.ID,
		Time:            in.Time,
		Extensions:      in.Extensions,
		DataContentType: in.DataContentType,
	}
	schema, err := decodeDataSchema(in.DataSchema)
	if err != nil {
		return err
	}
	m.DataSchema = schema

	return nil
}

// decodeDataSchema decodes the dataschema attribute in either persisted shape.
//
// The current shape is a URI string. The legacy shape is a marshaled url.URL
// struct, written by the plain-struct marshaling that preceded MarshalJSON, and it
// must still decode: an outbox row is durable, and rejecting the old shape would
// classify every such row as poison and stop the relay lane at the first one. The
// legacy value is lossy by construction (that is the bug this marshaler fixes), so
// it is reassembled from the struct's own fields — Userinfo cannot be recovered,
// which is exactly the information the old shape destroyed.
func decodeDataSchema(raw json.RawMessage) (*url.URL, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return nil, nil
	}

	if trimmed[0] == '"' {
		var s string
		if err := json.Unmarshal(trimmed, &s); err != nil {
			return nil, fmt.Errorf("event: unmarshal dataschema: %w", err)
		}
		if s == "" {
			return nil, nil
		}

		u, err := url.Parse(s)
		if err != nil {
			return nil, fmt.Errorf("event: parse dataschema %q: %w", s, err)
		}

		return u, nil
	}

	// Legacy shape: the url.URL struct itself.
	var legacy url.URL
	if err := json.Unmarshal(trimmed, &legacy); err != nil {
		return nil, fmt.Errorf("event: unmarshal legacy dataschema: %w", err)
	}
	// Drop the empty Userinfo the legacy encoding always produced; leaving it
	// non-nil is what made URL.String() emit a spurious "//@" authority.
	if legacy.User != nil && legacy.User.String() == "" {
		legacy.User = nil
	}
	if legacy == (url.URL{}) {
		return nil, nil
	}

	return &legacy, nil
}

// coreAttributeNames are the CloudEvents context attributes the envelope owns.
// Transports serialize these alongside extensions, so an extension may not reuse
// one of these names.
var coreAttributeNames = map[string]struct{}{
	"specversion":     {},
	"id":              {},
	"type":            {},
	"source":          {},
	"data":            {},
	"datacontenttype": {},
	"dataschema":      {},
	"subject":         {},
	"time":            {},
}

// BinaryAttrPrefix is the namespace binary content mode gives core attributes
// (RabbitMQ headers: "cloudEvents:id" and friends). Extensions travel un-prefixed,
// so an extension using this prefix would land on a core attribute.
//
// It is exported because the binary marshaler builds its header keys from it and
// ReservedExtensionName rejects extensions that use it: one definition, so the
// namespace a transport writes and the namespace publish defends cannot drift.
const BinaryAttrPrefix = "cloudEvents:"

// ReservedExtensionName reports whether name would collide with a core CloudEvents
// attribute in some serialization, making it illegal as an extension name.
//
// The check is deliberately the UNION over content modes rather than per-mode.
// Structured mode puts extensions in the same flat JSON object as the core
// attributes, so a bare "source" overwrites one; binary mode namespaces the core
// attributes under "cloudEvents:", so a "cloudEvents:id" extension overwrites one
// there instead. A publisher does not choose the mode its consumers use — and over
// an outbox the metadata is persisted and may be relayed by a transport the
// publisher never saw — so a name that is unsafe in EITHER mode has to be rejected
// in both.
//
// CloudEvents forbids such names outright, so this is a publish-time contract check,
// not a policy choice.
//
// The comparison is EXACT, not case-folded. Every serialization this repo emits is
// case-sensitive — structured mode writes the extension's own key into the JSON
// object, binary mode writes it as a bare AMQP header — so "Source" lands beside
// "source" rather than on top of it and corrupts nothing. Case-folding here
// rejected names that neither marshaler would have corrupted, and did it at the
// worst possible moment: over an outbox, publish runs inside the caller's business
// transaction, so an audit extension named "Type" failed every request.
func ReservedExtensionName(name string) bool {
	if strings.HasPrefix(name, BinaryAttrPrefix) {
		return true
	}
	_, core := coreAttributeNames[name]

	return core
}

// ValidExtensionValue reports whether v is an extension value every transport here
// can actually serialize, returning a descriptive error when it is not.
//
// The accepted set is a SUBSET of the CloudEvents extension-value types that every
// transport here round-trips: strings, booleans, the AMQP-representable integer and
// float widths, binary, and timestamps. Structured mode would take anything
// json.Marshal takes, but the mode is the CONSUMER's choice and the publisher does
// not know it — and over an outbox the metadata is persisted and may be relayed by a
// transport the publisher never saw.
//
// It is deliberately NARROWER than what amqp091-go's field encoder accepts. That
// encoder also writes nil ('V'), []any ('A') and amqp.Table ('F'), and those are
// rejected here anyway: a nil extension is indistinguishable from an absent one on
// the far side, and a nested value has no CloudEvents meaning while forcing every
// consumer to guess at a shape the spec does not define.
//
// A nested value (map, struct, slice) is the case that matters: amqp091-go's field
// encoder matches only the defined amqp.Table type, so a map[string]any extension
// fails there, and it fails at SEND time — after the outbox row committed with the
// caller's business transaction, as a non-DecodeError that no classifier claims. The
// lane then stops on that row every tick forever. Rejecting the value at publish is
// the cheap end of that, exactly as ReservedExtensionName is for names.
//
// A plain `int` is RANGE-checked rather than simply accepted. amqp091-go writes Go's
// `int` as a 32-bit signed AMQP field ('I', see its write.go), so a value above
// MaxInt32 is silently truncated on a 64-bit host — WithEventExtension("account",
// 3_000_000_000) arrives as -1294967296, with the publisher told the value was safe.
// An out-of-range int is therefore an error naming int64, which the encoder writes
// as a 64-bit field ('l').
func ValidExtensionValue(v any) error {
	switch v := v.(type) {
	case string, bool,
		int8, int16, int32, int64,
		uint8, uint16, uint32,
		float32, float64,
		[]byte, time.Time:
		return nil
	case int:
		if v > math.MaxInt32 || v < math.MinInt32 {
			return fmt.Errorf("int value %d does not fit the 32-bit AMQP field an untyped int is "+
				"encoded as (it would be silently truncated on the wire); use an int64 value instead", v)
		}

		return nil
	case nil:
		return errors.New("value is nil; omit the extension instead")
	default:
		return fmt.Errorf("value of type %T is not a CloudEvents extension value "+
			"(want a string, bool, integer, float, []byte or time.Time; nested maps, "+
			"structs and slices cannot be carried in binary content mode)", v)
	}
}

// ErrUnsendable marks metadata no transport here can turn into a message — a
// malformed event type, a reserved extension name, an extension value the wire
// format cannot carry. It is a property of the VALUE, so retrying with the same
// metadata can never succeed.
//
// It exists so a relay can tell that class apart from downstream trouble without
// a caller-supplied classifier. Publish-time validation (eventbus.publish,
// outbox.ValidateMetadata) keeps such metadata from being persisted in the first
// place, but an outbox row is durable: rows written before those checks existed,
// or by an older version of this library, still arrive at a Sender that must
// reject them. Without a marker the rejection is an opaque error no classifier
// claims, and the relay lane stops on that row every tick forever with nothing
// behind it delivered. With it, a relay configured with a PoisonHandler parks the
// row and moves on.
var ErrUnsendable = errors.New("event: metadata cannot be serialized for delivery")

// NewMetadata returns a Metadata for event type t with SpecVersion pinned to
// CloudEvents 1.0; every other field is left for the caller (or the bus's
// metadata completion) to fill.
func NewMetadata(t string) *Metadata {
	return &Metadata{
		SpecVersion: "1.0",
		Type:        t,
	}
}

type mdIncomingKey struct{}

// NewIncomingContext returns a ctx carrying md. Transports call it before
// invoking subscriber handlers, so handler code can recover the envelope via
// MetadataFromIncomingContext.
func NewIncomingContext(ctx context.Context, md *Metadata) context.Context {
	return context.WithValue(ctx, mdIncomingKey{}, md)
}

// MetadataFromIncomingContext returns the incoming event's Metadata stored in
// ctx by NewIncomingContext, and whether one was present.
func MetadataFromIncomingContext(ctx context.Context) (*Metadata, bool) {
	md, ok := ctx.Value(mdIncomingKey{}).(*Metadata)
	if !ok {
		return nil, false
	}

	return md, true
}
