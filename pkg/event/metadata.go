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
// unchanged from the plain-struct marshaling this replaces. DataSchema is decoded
// through a json.RawMessage rather than a string because its VALUE shape did
// change: rows persisted before this marshaler existed hold the url.URL struct.
// Rejecting those would turn every one of them into a poison row and stop the
// relay lane at it, so the decoder accepts both (see decodeDataSchema).
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

// MarshalJSON implements json.Marshaler. See metadataJSON.
func (m Metadata) MarshalJSON() ([]byte, error) {
	out := metadataJSON{
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
		// Both checks reject rather than encode, and for the same reason: an outbox
		// row is durable, so anything written here must be readable back. A value
		// that is not is worse than a failed publish — it is a row that decodes to
		// something different, or not at all, on every future read.
		uri := m.DataSchema.String()
		switch uri {
		case "":
			// An empty URI decodes back to a NIL DataSchema, so the same event would
			// carry a non-nil schema in-process and a nil one after crossing a store,
			// and `if md.DataSchema != nil` would branch differently on the two
			// sides. An empty URI reference is not a valid dataschema anyway — a
			// &url.URL{} here is a caller mistake worth naming.
			return nil, errors.New("event: DataSchema is non-nil but its URI is empty; leave it nil instead")
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

		schema, err := json.Marshal(uri)
		if err != nil {
			return nil, fmt.Errorf("event: marshal dataschema: %w", err)
		}
		out.DataSchema = schema
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

// binaryAttrPrefix is the namespace binary content mode gives core attributes
// (RabbitMQ headers: "cloudEvents:id" and friends). Extensions travel un-prefixed,
// so an extension using this prefix would land on a core attribute.
const binaryAttrPrefix = "cloudEvents:"

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
func ReservedExtensionName(name string) bool {
	if strings.HasPrefix(name, binaryAttrPrefix) {
		return true
	}
	_, core := coreAttributeNames[strings.ToLower(name)]

	return core
}

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
