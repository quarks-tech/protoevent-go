package binary

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"

	"github.com/quarks-tech/protoevent-go/pkg/event"
	"github.com/quarks-tech/protoevent-go/pkg/transport/rabbitmq/internal/consume"
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

// The core CloudEvents attributes, as the header keys binary mode actually writes:
// the namespace prefix comes from event.BinaryAttrPrefix, so the names this
// marshaler writes and the names event.ReservedExtensionName defends cannot drift.
//
// They are full keys, not a prefix plus a name assembled at each use, so no lookup
// concatenates strings at run time.
const (
	hdrSpecVersion = event.BinaryAttrPrefix + "specversion"
	hdrID          = event.BinaryAttrPrefix + "id"
	hdrSource      = event.BinaryAttrPrefix + "source"
	hdrSubject     = event.BinaryAttrPrefix + "subject"
	hdrTime        = event.BinaryAttrPrefix + "time"
	hdrDataSchema  = event.BinaryAttrPrefix + "dataschema"
)

func marshalMetadata(meta *event.Metadata) (amqp.Table, error) {
	headers := amqp.Table{
		hdrSpecVersion: meta.SpecVersion,
		hdrID:          meta.ID,
		hdrSource:      meta.Source,
	}

	if meta.Subject != "" {
		headers[hdrSubject] = meta.Subject
	}

	if meta.DataSchema != nil {
		headers[hdrDataSchema] = meta.DataSchema.String()
	}

	if !meta.Time.IsZero() {
		headers[hdrTime] = meta.Time.Format(time.RFC3339)
	}

	// Rejected, not merged — the same rule structured mode enforces, and it matters
	// MORE here because binary is the default content mode. Copying extensions over
	// the table would let WithEventExtension("cloudEvents:id", …) replace the core
	// attribute, and the consumer reads its Metadata.ID — its dedup key — straight
	// back out of that header. Extensions own the un-prefixed namespace; the
	// prefixed one belongs to the envelope.
	//
	// The name check is event.ReservedExtensionName, the union over content modes,
	// so it also rejects a bare "source" here even though binary mode alone would
	// survive it: a publisher does not choose the mode its consumers use, and over an
	// outbox the metadata is persisted and may be relayed by a transport the
	// publisher never saw. See its doc.
	for k, v := range meta.Extensions {
		if event.ReservedExtensionName(k) {
			return nil, fmt.Errorf(
				"extension %q is a reserved CloudEvents attribute name and would overwrite a core attribute; rename the extension", k)
		}

		// A nested extension value otherwise reaches amqp091-go's field encoder, which
		// matches only the defined AMQP field types, and fails at SEND time — over an
		// outbox that is after the row committed with the caller's business
		// transaction, leaving the lane stuck on that row every tick.
		if err := event.ValidExtensionValue(v); err != nil {
			return nil, fmt.Errorf("extension %q: %w", k, err)
		}

		headers[k] = v
	}

	return headers, nil
}

// require extracts a mandatory cloudEvents header, named by its full key; name is
// the short attribute name the error reports. A MISSING or wrong-typed header is an
// error, matching the d.Type guard in unmarshalMetadata.
//
// A present-but-empty one is not. The per-header blocks this replaced accepted it,
// so rejecting it turns a publisher that worked before the upgrade into one whose
// every delivery is dead-lettered as unprocessable — a compatibility break for no
// gain, since an empty required attribute is the publisher's problem and travels
// visibly in the metadata either way.
func require(headers amqp.Table, key, name string) (string, error) {
	v, ok := consume.HeaderString(headers[key])
	if !ok {
		return "", fmt.Errorf("required attribute '%s' is missing", name)
	}

	return v, nil
}

func unmarshalMetadata(d *amqp.Delivery) (*event.Metadata, error) {
	if d.Type == "" {
		return nil, errors.New("required attribute 'type' is missing")
	}

	md := &event.Metadata{
		DataContentType: d.ContentType,
		Type:            d.Type,
	}

	var err error
	if md.SpecVersion, err = require(d.Headers, hdrSpecVersion, "specversion"); err != nil {
		return nil, err
	}
	if md.ID, err = require(d.Headers, hdrID, "id"); err != nil {
		return nil, err
	}
	if md.Source, err = require(d.Headers, hdrSource, "source"); err != nil {
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
	if v, ok := consume.HeaderString(d.Headers[hdrSubject]); ok && v != "" {
		md.Subject = v
	}

	if v, ok := consume.HeaderString(d.Headers[hdrTime]); ok && v != "" {
		var err error

		md.Time, err = time.Parse(time.RFC3339, v)
		if err != nil {
			return nil, fmt.Errorf("parse attribute 'time': %w", err)
		}
	}

	if v, ok := consume.HeaderString(d.Headers[hdrDataSchema]); ok && v != "" {
		var err error

		md.DataSchema, err = url.Parse(v)
		if err != nil {
			return nil, fmt.Errorf("parse attribute 'dataschema': %w", err)
		}
	}

	for k, v := range d.Headers {
		if !strings.HasPrefix(k, event.BinaryAttrPrefix) {
			if md.Extensions == nil {
				md.Extensions = make(map[string]any)
			}

			md.Extensions[k] = v
		}
	}

	return md, nil
}
