package structured

import (
	"bytes"
	"errors"
	"fmt"
	"net/url"
	"time"

	json "github.com/json-iterator/go"
	amqp "github.com/rabbitmq/amqp091-go"

	"github.com/quarks-tech/protoevent-go/pkg/event"
)

type Marshaler struct{}

func (m Marshaler) Marshal(md *event.Metadata, data []byte) (amqp.Publishing, error) {
	dto := map[string]any{
		"specversion": md.SpecVersion,
		"id":          md.ID,
		"type":        md.Type,
		"source":      md.Source,
		"data":        json.RawMessage(data),
	}

	if md.DataContentType != "" {
		dto["datacontenttype"] = md.DataContentType
	}

	if md.DataSchema != nil {
		dto["dataschema"] = md.DataSchema.String()
	}

	if md.Subject != "" {
		dto["subject"] = md.Subject
	}

	if !md.Time.IsZero() {
		dto["time"] = md.Time.Format(time.RFC3339)
	}

	// Extensions share one flat namespace with the core attributes in structured
	// mode (binary mode does not: it prefixes core attributes with cloudEvents:).
	// A collision must therefore be REJECTED, not merged: writing extensions over
	// the map would let WithEventExtension("source", …) — or worse, ("data", …) —
	// replace a core attribute, and the corruption only appears after a switch to
	// structured mode. The consumer would then route on an attacker- or
	// accident-supplied type, or decode the extension value as the payload, with
	// nothing in the resulting error naming the extension. CloudEvents forbids
	// extension names that collide with core attributes, so this is a publish-time
	// contract violation.
	//
	// The name check is event.ReservedExtensionName, the union over content modes, so
	// it also rejects a "cloudEvents:"-prefixed extension here even though this
	// envelope alone would survive it: a publisher does not choose the mode its
	// consumers use, and over an outbox the metadata is persisted and may be relayed
	// by a transport the publisher never saw. See its doc.
	for k, v := range md.Extensions {
		if event.ReservedExtensionName(k) {
			return amqp.Publishing{}, fmt.Errorf(
				"extension %q is a reserved CloudEvents attribute name and would overwrite a core attribute; rename the extension", k)
		}

		dto[k] = v
	}

	body, err := json.Marshal(&dto)
	if err != nil {
		return amqp.Publishing{}, fmt.Errorf("marshal cloudevents envelope: %w", err)
	}

	return amqp.Publishing{
		Type:        md.Type,
		ContentType: md.DataContentType,
		Body:        body,
	}, nil
}

// errNotAString marks a value that carries no string at all (JSON null, or an
// empty payload) — as opposed to one that is the wrong shape. Required attributes
// reject both; optional ones treat this one as "not set".
var errNotAString = errors.New("must be a string, got null or nothing")

// decodeString decodes one string-valued CloudEvents envelope attribute.
//
// A JSON string is decoded through json.Unmarshal, so escape sequences are
// interpreted — the quote-trimming this replaces left "a\"b" as the literal a\"b
// and cheerfully "unquoted" an object into its raw JSON text.
//
// JSON numbers and booleans are accepted and taken verbatim. Every one of these
// attributes is a string in the CloudEvents spec, and a strict decoder is
// defensible — but publishers in other languages do emit `"id": 12345`, and the
// pre-existing quote-trimming accepted them (as "12345"). Rejecting them here
// would make a publisher that worked before this change unconsumable, with each
// delivery failing as unprocessable. Tolerating a scalar costs nothing and keeps
// the wire contract compatible.
//
// null, objects and arrays carry no string value and are errors. null (and an
// empty payload) is reported as errNotAString specifically, so that the OPTIONAL
// attributes can treat it as "absent" rather than as malformed input.
func decodeString(raw json.RawMessage) (string, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return "", errNotAString
	}

	switch trimmed[0] {
	case '"':
		var value string
		if err := json.Unmarshal(trimmed, &value); err != nil {
			return "", fmt.Errorf("must be a string: %w", err)
		}

		return value, nil
	case '{', '[':
		return "", errors.New("must be a string, got an object or array")
	default:
		// Number or boolean literal: the source text IS the value.
		return string(trimmed), nil
	}
}

// decoder reads attributes out of a decoded CloudEvents envelope, consuming each
// one it reads so that whatever is left in dto is exactly the extension set.
type decoder struct {
	dto map[string]json.RawMessage
}

// require extracts (and consumes) a mandatory envelope attribute.
//
// A missing attribute, and one carrying no string at all (null, an object, an
// array), are errors. A present-but-EMPTY string is not: the quote-trimming path
// this replaced accepted it, and binary mode's own `require` still does, so
// rejecting it here dead-lettered 100% of the traffic from any publisher that
// emits every field unconditionally — generated serializers in other languages
// do — from the moment the consumer was upgraded, with nothing surfacing on the
// publisher side. An empty required attribute is the publisher's problem and
// travels visibly in the metadata either way; the two content modes disagreeing
// about identical input is this library's problem.
func (d decoder) require(key string) (string, error) {
	raw, ok := d.dto[key]
	if !ok {
		return "", fmt.Errorf("required attribute '%s' is missing", key)
	}
	delete(d.dto, key)

	value, err := decodeString(raw)
	if err != nil {
		return "", fmt.Errorf("required attribute '%s': %w", key, err)
	}

	return value, nil
}

// optional extracts (and consumes) an OPTIONAL string-valued attribute
// through the same decoder, so every attribute in the envelope is parsed one
// way. The old quote-trimming path left here interpreted no escapes (a
// subject of "a\"b" arrived as a\"b) and silently "unquoted" objects and
// arrays into their raw JSON text.
//
// An OPTIONAL attribute must never fail the delivery on account of being
// absent-shaped: JSON `null` and an empty string both mean "not set" and are
// reported as absent, exactly as the quote-trimming path effectively did. A
// publisher that emits `"subject": null` — common in generated serializers that
// always write every field — would otherwise have every one of its events
// dead-lettered. A non-scalar value (object/array) is still an error: there is
// no string in it to use, and silently accepting its raw JSON text is what
// produced garbage metadata before.
func (d decoder) optional(key string) (string, bool, error) {
	raw, ok := d.dto[key]
	if !ok {
		return "", false, nil
	}
	delete(d.dto, key)

	value, err := decodeString(raw)
	switch {
	case errors.Is(err, errNotAString):
		return "", false, nil // null: absent, not malformed
	case err != nil:
		return "", false, fmt.Errorf("attribute '%s': %w", key, err)
	case value == "":
		return "", false, nil
	}

	return value, true, nil
}

func (m Marshaler) Unmarshal(d *amqp.Delivery) (*event.Metadata, []byte, error) {
	dto := make(map[string]json.RawMessage)

	if err := json.Unmarshal(d.Body, &dto); err != nil {
		return nil, nil, fmt.Errorf("unmarshal cloudevents envelope: %w", err)
	}

	md := new(event.Metadata)

	// The leftover dto entries become extensions below, so every read below goes
	// through dec, which consumes what it reads.
	dec := decoder{dto: dto}

	var err error

	if md.SpecVersion, err = dec.require("specversion"); err != nil {
		return nil, nil, err
	}
	if md.Type, err = dec.require("type"); err != nil {
		return nil, nil, err
	}
	if md.ID, err = dec.require("id"); err != nil {
		return nil, nil, err
	}
	if md.Source, err = dec.require("source"); err != nil {
		return nil, nil, err
	}

	subject, ok, err := dec.optional("subject")
	if err != nil {
		return nil, nil, err
	}
	if ok {
		md.Subject = subject
	}

	// `time` gets the stricter treatment: JSON null still reads as absent (a
	// serializer that writes every field is not malformed input), but an
	// explicitly-present EMPTY string is not a timestamp and is not "absent"
	// either. Accepting it would hand the subscriber a zero Metadata.Time, which is
	// the exact value the outbox read path uses as its poison marker — so a
	// malformed timestamp would propagate instead of failing at the boundary.
	if raw, present := dto["time"]; present {
		delete(dto, "time")

		rawTime, err := decodeString(raw)
		switch {
		case errors.Is(err, errNotAString):
			// null: absent, per the optional-attribute contract.
		case err != nil:
			return nil, nil, fmt.Errorf("attribute 'time': %w", err)
		default:
			if md.Time, err = time.Parse(time.RFC3339, rawTime); err != nil {
				return nil, nil, fmt.Errorf("parse attribute 'time': %w", err)
			}
		}
	}

	dataSchema, ok, err := dec.optional("dataschema")
	if err != nil {
		return nil, nil, err
	}
	if ok {
		if md.DataSchema, err = url.Parse(dataSchema); err != nil {
			return nil, nil, fmt.Errorf("parse attribute 'dataschema': %w", err)
		}
	}

	dataContentType, ok, err := dec.optional("datacontenttype")
	if err != nil {
		return nil, nil, err
	}
	if ok {
		md.DataContentType = dataContentType
	}

	// data is required too, but stays raw bytes: no quote-trimming.
	rawData, ok := dto["data"]
	if !ok {
		return nil, nil, errors.New("required attribute 'data' is missing")
	}
	data := []byte(rawData)
	delete(dto, "data")

	for k, raw := range dto {
		if md.Extensions == nil {
			md.Extensions = make(map[string]any)
		}

		var v any

		if err := json.Unmarshal(raw, &v); err != nil {
			return nil, nil, fmt.Errorf("unmarshal extension attribute %q: %w", k, err)
		}

		md.Extensions[k] = v
	}

	return md, data, nil
}
