package outbox

import (
	"time"

	"github.com/quarks-tech/protoevent-go/pkg/event"
)

// Message represents an event stored in the outbox log.
type Message struct {
	// ID is the unique identifier of the outbox row (primary key).
	ID string

	// Seq is the logical log offset assigned by the sequencer after commit.
	// Zero until sequenced; set by the relay store on read. Only the
	// sequenced-log (TiDB) runtime uses it — the MongoDB change-stream runtime
	// orders by oplog commit order and always leaves Seq zero.
	Seq int64

	// Metadata contains CloudEvents metadata.
	//
	// Note on Extensions: stores persist Metadata as JSON, so extension values
	// round-trip through encoding/json — numeric extension values come back as
	// float64 regardless of the type the publisher set. Consumers needing
	// exact numeric types should encode them as strings.
	Metadata *event.Metadata

	// Data is the serialized event payload.
	Data []byte

	// CreateTime is the insertion-time anchor. The Sender stamps it at
	// publish; the MongoDB store persists that value (see its clock caveat),
	// while the TiDB store ignores it and re-stamps from the database clock
	// on insert. Distinct from Metadata.Time (the event's occurred-at, which
	// publishers may backdate). Used as the age anchor for drain-lag
	// observability and insert-time retention.
	CreateTime time.Time
}
