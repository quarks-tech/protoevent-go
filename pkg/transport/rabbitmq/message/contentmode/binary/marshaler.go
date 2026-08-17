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
	// Both rejections wrap event.ErrUnsendable. They are properties of the
	// METADATA, so retrying is futile — and an outbox row persisted before
	// publish-time validation existed reaches this exact point, where an
	// unclassified error stops the relay lane forever. See event.ErrUnsendable.
	for k, v := range meta.Extensions {
		if event.ReservedExtensionName(k) {
			return nil, fmt.Errorf(
				"%w: extension %q is a reserved CloudEvents attribute name and would overwrite a core attribute; rename the extension",
				event.ErrUnsendable, k)
		}

		// A nested extension value otherwise reaches amqp091-go's field encoder, which
		// matches only the defined AMQP field types, and fails at SEND time — over an
		// outbox that is after the row committed with the caller's business
		// transaction, leaving the lane stuck on that row every tick.
		if err := event.ValidExtensionValue(v); err != nil {
			return nil, fmt.Errorf("%w: extension %q: %w", event.ErrUnsendable, k, err)
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

	// The STRING-valued optional attributes treat a present-but-empty header as
	// ABSENT, matching the structured mode's `optional` helper. A publisher that
	// emits every header unconditionally is not sending malformed input, and for
	// these the empty string genuinely means "not set":
	//
	//   - an empty subject is no subject;
	//   - an empty dataschema is no schema — and taking it literally is actively
	//     harmful, since url.Parse("") succeeds and yields a NON-NIL &url.URL{},
	//     which event.Metadata.MarshalJSON rejects, so re-publishing such an event
	//     through an outbox store fails inside the caller's business transaction
	//     and rolls the whole request back.
	//
	// `time` is the exception, and it must stay in lockstep with structured mode
	// (see the matching comment there). It is a TYPED attribute: an empty string
	// is not a timestamp and is not "absent" either. Mapping it to absent hands
	// the subscriber a zero Metadata.Time — the exact value the outbox read path
	// uses as its poison marker — so the malformed value would survive this
	// boundary only to fail later, either inside a caller's business transaction
	// via outbox.ValidateMetadata or as a poison row on the relay's read path.
	// Failing here dead-letters one delivery instead.
	if v, ok := consume.HeaderString(d.Headers[hdrSubject]); ok && v != "" {
		md.Subject = v
	}

	if v, ok := consume.HeaderString(d.Headers[hdrTime]); ok {
		if v == "" {
			return nil, errors.New("attribute 'time' is present but empty; omit the header instead")
		}

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

	// Only headers that are actually CloudEvents extensions are lifted into
	// Extensions, and the filter is the SAME pair of rules marshalMetadata
	// enforces on the way out — so Marshal(Unmarshal(d)) stays closed.
	//
	// Copying every non-prefixed header, which is what this used to do, broke that
	// as soon as marshalMetadata started rejecting instead of merging. The broker
	// writes its own bookkeeping into the same flat namespace: a delivery that has
	// been dead-lettered once carries `x-death`, an []any, so re-sending the
	// Metadata a subscriber just received failed with "value of type
	// []interface {} is not a CloudEvents extension value". Over an outbox that is
	// fatal rather than merely noisy — a marshal failure is not a *DecodeError, no
	// UnsendableClassifier claims it, and the lane stops on that row every tick
	// forever.
	//
	// Dropped silently, not reported: these are transport-layer headers that were
	// never part of the event, and the alternative — failing the delivery — would
	// dead-letter every once-retried message in the system. A publisher's own
	// extension is unaffected, because publish already rejects the names and
	// values this skips.
	for k, v := range d.Headers {
		if strings.HasPrefix(k, event.BinaryAttrPrefix) || event.ReservedExtensionName(k) {
			continue
		}
		if brokerDeathHeader(k) {
			continue
		}
		if err := event.ValidExtensionValue(v); err != nil {
			continue
		}
		if md.Extensions == nil {
			md.Extensions = make(map[string]any)
		}

		md.Extensions[k] = v
	}

	return md, nil
}

// brokerDeathHeader reports whether k is RabbitMQ's dead-lettering bookkeeping,
// which is written by the broker and is never part of the event.
//
// Matched by NAME, not by value type. The type check below this call already drops
// `x-death` — but only incidentally, because it is a []interface{} that
// ValidExtensionValue refuses. The `x-first-death-*` and `x-last-death-*` family are
// plain strings and sailed straight through, which made the type check a filter that
// only appeared to do what its comment said.
//
// Letting them through is not cosmetic. `x-first-death-queue` is the key the
// parking-lot receiver's retry budget is evaluated against
// (parkinglot.Receiver.hasExceededRetryCount): once promoted into Metadata.Extensions
// it is persisted by an outbox and re-emitted as a header, so a downstream receiver
// looks for ANOTHER service's queue name among its own x-death entries, never finds
// it, and applies no cap — the delivery loops through the wait queue indefinitely,
// re-running side effects. Re-publishing an incoming Metadata is the documented
// forward pattern, so this needs no malice to happen.
func brokerDeathHeader(k string) bool {
	return k == "x-death" ||
		strings.HasPrefix(k, "x-first-death-") ||
		strings.HasPrefix(k, "x-last-death-")
}
