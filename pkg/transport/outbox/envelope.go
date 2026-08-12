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
// message must carry metadata, and that metadata must carry a time.
//
// The zero-time rejection is the write half of UnmarshalMetadata's poison rule.
// It also keeps a zero time out of SQL DATETIME columns, where 0001-01-01 is
// below the minimum and surfaces as an opaque driver error from inside the
// caller's business transaction.
func ValidateMetadata(md *event.Metadata) error {
	if md == nil {
		return errors.New("outbox: message metadata is nil")
	}
	if md.Time.IsZero() {
		return errors.New("outbox: message metadata time is zero; set Metadata.Time before publishing")
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
