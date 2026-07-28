package binary

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"

	"github.com/quarks-tech/protoevent-go/pkg/event"
)

type Marshaler struct{}

func (m Marshaler) Marshal(md *event.Metadata, data []byte) (amqp.Publishing, error) {
	headers, err := marshalMetadata(md)
	if err != nil {
		return amqp.Publishing{}, err
	}

	return amqp.Publishing{
		Type:        md.Type,
		ContentType: md.DataContentType,
		Headers:     headers,
		Body:        data,
	}, nil
}

func (m Marshaler) Unmarshal(d *amqp.Delivery) (*event.Metadata, []byte, error) {
	md, err := unmarshalMetadata(d)
	if err != nil {
		return nil, nil, fmt.Errorf("parse amqp delivery: %w", err)
	}

	return md, d.Body, nil
}

// attrPrefix namespaces the core CloudEvents attributes in binary mode. Extensions
// travel under their bare names, so an extension may not use this prefix.
const attrPrefix = "cloudEvents:"

func marshalMetadata(meta *event.Metadata) (amqp.Table, error) {
	headers := amqp.Table{
		attrPrefix + "specversion": meta.SpecVersion,
		attrPrefix + "id":          meta.ID,
		attrPrefix + "source":      meta.Source,
	}

	if meta.Subject != "" {
		headers[attrPrefix+"subject"] = meta.Subject
	}

	if meta.DataSchema != nil {
		headers[attrPrefix+"dataschema"] = meta.DataSchema.String()
	}

	if !meta.Time.IsZero() {
		headers[attrPrefix+"time"] = meta.Time.Format(time.RFC3339)
	}

	// Rejected, not merged — the same rule structured mode enforces, and it matters
	// MORE here because binary is the default content mode. Copying extensions over
	// the table would let WithEventExtension("cloudEvents:id", …) replace the core
	// attribute, and the consumer reads its Metadata.ID — its dedup key — straight
	// back out of that header. Extensions own the un-prefixed namespace; the
	// prefixed one belongs to the envelope.
	for k, v := range meta.Extensions {
		if strings.HasPrefix(k, attrPrefix) {
			return nil, fmt.Errorf(
				"extension %q uses the reserved %q prefix, which would overwrite a core CloudEvents attribute; rename the extension",
				k, attrPrefix)
		}
		headers[k] = v
	}

	return headers, nil
}

func unmarshalMetadata(d *amqp.Delivery) (*event.Metadata, error) {
	if d.Type == "" {
		return nil, errors.New("required attribute 'type' is missing")
	}

	md := &event.Metadata{
		DataContentType: d.ContentType,
		Type:            d.Type,
	}

	// require extracts a mandatory cloudEvents header. A MISSING or wrong-typed
	// header is an error, matching the d.Type guard above.
	//
	// A present-but-empty one is not. The per-header blocks this replaced accepted
	// it, so rejecting it turns a publisher that worked before the upgrade into one
	// whose every delivery is dead-lettered as unprocessable — a compatibility break
	// for no gain, since an empty required attribute is the publisher's problem and
	// travels visibly in the metadata either way. Structured mode's `require` is
	// stricter because there the value shape is genuinely ambiguous (null vs "" vs
	// absent in JSON); an AMQP header has no such ambiguity.
	require := func(key string) (string, error) {
		v, ok := d.Headers[attrPrefix+key].(string)
		if !ok {
			return "", fmt.Errorf("required attribute '%s' is missing", key)
		}
		return v, nil
	}

	var err error
	if md.SpecVersion, err = require("specversion"); err != nil {
		return nil, err
	}
	if md.ID, err = require("id"); err != nil {
		return nil, err
	}
	if md.Source, err = require("source"); err != nil {
		return nil, err
	}

	// The OPTIONAL attributes treat a present-but-empty header as ABSENT, matching
	// the structured mode's `optional` helper. A publisher that emits every header
	// unconditionally is not sending malformed input, and the two modes disagreeing
	// on it is a bug that depends only on which mode the upstream chose:
	//
	//   - an empty dataschema through url.Parse("") succeeds and yields a NON-NIL
	//     &url.URL{}, which event.Metadata.MarshalJSON now rejects — so
	//     re-publishing such an event through an outbox store fails inside the
	//     caller's business transaction and rolls the whole request back;
	//   - an empty time fails time.Parse and dead-letters the entire delivery.
	if v, ok := d.Headers[attrPrefix+"subject"].(string); ok && v != "" {
		md.Subject = v
	}

	if v, ok := d.Headers[attrPrefix+"time"].(string); ok && v != "" {
		var err error

		md.Time, err = time.Parse(time.RFC3339, v)
		if err != nil {
			return nil, fmt.Errorf("parse attribute 'time': %w", err)
		}
	}

	if v, ok := d.Headers[attrPrefix+"dataschema"].(string); ok && v != "" {
		var err error

		md.DataSchema, err = url.Parse(v)
		if err != nil {
			return nil, fmt.Errorf("parse attribute 'dataschema': %w", err)
		}
	}

	for k, v := range d.Headers {
		if !strings.HasPrefix(k, attrPrefix) {
			if md.Extensions == nil {
				md.Extensions = make(map[string]any)
			}

			md.Extensions[k] = v
		}
	}

	return md, nil
}
