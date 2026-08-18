package outbox

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"

	"github.com/quarks-tech/protoevent-go/pkg/event"
)

// ErrPoisonEnvelope marks persisted metadata that cannot be turned back into an
// event. A store returns it (wrapped in its own runtime's DecodeError) so the
// relay parks the row or stops the lane at it, rather than forwarding an event
// the row does not actually contain.
var ErrPoisonEnvelope = errors.New("outbox: persisted metadata is unusable")

// Compile-time pin on event.Metadata's OWN JSON codec, which is what the
// persisted envelope below depends on: a *url.URL does not survive the reflected
// struct encoding (see event.Metadata.MarshalJSON), so a DataSchema written that
// way is read back with a spurious "//@" authority — durably, in every relayed
// event.
//
// It has to be pinned at COMPILE time because nothing here NAMES the codec:
// json.Marshal accepts any value, so a build against a protoevent-go that
// predates the marshaler silently degrades to the corrupting encoding instead of
// failing. That build is not hypothetical — it is what an external
// `go get` of this module resolves whenever this go.mod's protoevent-go require
// is a tag older than the marshaler, and go.work hides it in-repo. With these
// assertions such a build fails and `make check-modules` reports it.
var (
	_ json.Marshaler   = event.Metadata{}
	_ json.Unmarshaler = (*event.Metadata)(nil)
)

// MarshalMetadata encodes event metadata for persistence.
//
// It lives here, in the module both backends depend on, rather than in each
// store: the encoding and the poison rules below are ONE contract about what an
// outbox row means, and two copies of it means two backends can disagree about
// whether the same row is deliverable or poison. Sibling store modules cannot
// import each other, so this is the only place the rule can be shared.
func MarshalMetadata(md *event.Metadata) ([]byte, error) {
	b, err := json.Marshal(md)
	if err != nil {
		return nil, fmt.Errorf("outbox: marshal metadata: %w", err)
	}

	return b, nil
}

// UnmarshalMetadata decodes persisted metadata and classifies what cannot be
// used as poison (an error wrapping ErrPoisonEnvelope).
//
// Three rules, each closing a way an unusable row could otherwise be delivered
// as if it were a real event:
//
//   - The decode target is a POINTER. JSON `null` unmarshals into a struct value
//     without error, leaving it zero, so a null row would slip past every check
//     and be sent downstream as an empty event. Into a pointer it yields nil.
//   - A nil result is poison, per the above.
//   - A zero Metadata.Time is poison. JSON-valid-but-empty metadata ("{}")
//     decodes into a non-nil value with every field zero; ValidateMetadata
//     rejects a zero Time on the write side, so such a row was not written by
//     this library and its contents cannot be trusted.
func UnmarshalMetadata(b []byte) (*event.Metadata, error) {
	var md *event.Metadata
	if err := json.Unmarshal(b, &md); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrPoisonEnvelope, err)
	}
	if md == nil {
		return nil, fmt.Errorf("%w: persisted metadata is JSON null", ErrPoisonEnvelope)
	}
	if md.Time.IsZero() {
		return nil, fmt.Errorf("%w: persisted metadata has zero time", ErrPoisonEnvelope)
	}

	return md, nil
}

// ValidateMetadata checks the write-side preconditions every backend shares: a
// message must carry metadata, that metadata must carry a time, its event type
// must have the shape a transport can route, and every extension must be one a
// transport can serialize.
//
// The zero-time rejection is the write half of UnmarshalMetadata's poison rule.
// It also keeps a zero time out of SQL DATETIME columns, where 0001-01-01 is
// below the minimum and surfaces as an opaque driver error from inside the
// caller's business transaction.
//
// The type and extension rules are the SAME ones eventbus.publish enforces, and
// they are repeated here rather than trusted to it because the outbox Sender is
// reachable without the bus: a forwarder that re-publishes an incoming
// event.Metadata straight through outboxSender.Send (the shape the README shows)
// never passes through publish at all. The consequence of letting one such row
// commit is not a failed publish but a wedged relay — the RabbitMQ sender needs
// the dot to split exchange from routing key and the AMQP field encoder refuses a
// nested extension, both at SEND time, and neither failure is a DecodeError any
// classifier claims, so the lane stops on that row every tick forever. Rejecting
// it here costs the caller's transaction one error instead.
func ValidateMetadata(md *event.Metadata) error {
	if md == nil {
		return errors.New("outbox: message metadata is nil")
	}
	if md.Time.IsZero() {
		return errors.New("outbox: message metadata time is zero; set Metadata.Time before publishing")
	}
	if _, _, err := event.SplitType(md.Type); err != nil {
		return fmt.Errorf("outbox: message metadata type: %w", err)
	}
	// The type becomes amqp.Publishing.Type, a shortstr — see event.MaxShortStrLen
	// for why a length this validation misses wedges a lane rather than failing a
	// publish.
	if event.ShortStrTooLong(md.Type) {
		return fmt.Errorf(
			"outbox: message metadata type is %d bytes, over the %d-byte AMQP limit; shorten the event type",
			len(md.Type), event.MaxShortStrLen)
	}
	for k, v := range md.Extensions {
		if event.ReservedExtensionName(k) {
			return fmt.Errorf("outbox: extension %q collides with a core CloudEvents attribute; rename it", k)
		}
		// An extension name becomes an AMQP header-table key, also a shortstr.
		if event.ShortStrTooLong(k) {
			return fmt.Errorf(
				"outbox: extension name is %d bytes, over the %d-byte AMQP limit; shorten it",
				len(k), event.MaxShortStrLen)
		}
		if err := validExtension(v); err != nil {
			return fmt.Errorf("outbox: extension %q: %w", k, err)
		}
	}

	return nil
}

// validExtension applies both extension-value rules: the transports' shared type
// rule, and this envelope's narrower exact-integer rule.
func validExtension(v any) error {
	if err := event.ValidExtensionValue(v); err != nil {
		return err
	}

	return exactInEnvelope(v)
}

// maxExactEnvelopeInt is the largest integer the outbox envelope round-trips
// exactly: encoding/json unmarshals every JSON number into float64, whose
// mantissa holds 53 bits.
const maxExactEnvelopeInt = int64(1) << 53

// exactInEnvelope rejects an extension value whose VALUE cannot survive the JSON
// envelope, as opposed to one whose Go type merely changes.
//
// event.ValidExtensionValue accepts int64 at any magnitude, and for its own
// purpose that is correct — the AMQP encoder writes int64 as a 64-bit field, so a
// direct publish loses nothing. The outbox is a narrower channel: the same value
// stored in a row and read back comes through float64 and is silently off by
// however much 53 bits could not hold. Rejecting it costs the caller's transaction
// one error; accepting it corrupts a durable record with nothing anywhere
// reporting it. See TestEnvelopeRoundTripLosesLargeIntegerExtensions.
//
// Only int64 needs the check: ValidExtensionValue already caps plain int to the
// int32 range, and every other accepted numeric type fits in 53 bits. []byte and
// time.Time are deliberately NOT rejected — their values survive as base64 and
// RFC3339 and only the Go type changes, which loses no data.
func exactInEnvelope(v any) error {
	n, ok := v.(int64)
	if !ok {
		return nil
	}
	if n > maxExactEnvelopeInt || n < -maxExactEnvelopeInt {
		return fmt.Errorf("int64 value %d cannot survive the outbox envelope: it is persisted as "+
			"JSON and read back through float64, which represents integers exactly only up to "+
			"%d, so the stored row would differ from what was published; carry it as a string",
			n, maxExactEnvelopeInt)
	}

	return nil
}

// MaxInstancePrefixLen bounds an instance prefix. It keeps every prefixed
// identifier — including the per-instance golang-migrate versions table (prefix
// + "schema_migrations") — comfortably under TiDB's 64-character identifier
// limit, and it applies to every backend so that one prefix value works verbatim
// across all of them.
const MaxInstancePrefixLen = 40

// instancePrefixPattern constrains a prefix to a safe identifier fragment: SQL
// backends splice it into DDL/DML as an IDENTIFIER (it cannot be a bound
// parameter), so it must never carry quoting or punctuation.
var instancePrefixPattern = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]*$`)

// ValidateInstancePrefix checks the prefix that names one outbox instance —
// TiDB's table prefix, MongoDB's collection prefix — so that the same value is
// accepted or rejected identically by every backend. kind names the flavor in
// error messages ("table prefix", "collection prefix").
//
// Shared deliberately: the prefix is how several independent outboxes coexist in
// one database, and a rule that drifted between backends would make a prefix
// that works on one panic at process start on the other.
func ValidateInstancePrefix(kind, prefix string) error {
	if prefix == "" {
		return fmt.Errorf("outbox: %s must not be empty", kind)
	}
	if len(prefix) > MaxInstancePrefixLen {
		return fmt.Errorf("outbox: %s %q exceeds %d characters", kind, prefix, MaxInstancePrefixLen)
	}
	if !instancePrefixPattern.MatchString(prefix) {
		return fmt.Errorf("outbox: %s %q is not a valid identifier fragment (want %s)", kind, prefix, instancePrefixPattern)
	}

	return nil
}
