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
	// Zero until sequenced; set by the relay store on read.
	Seq int64

	// Metadata contains CloudEvents metadata.
	Metadata *event.Metadata

	// Data is the serialized event payload.
	Data []byte

	// CreateTime is when the message was inserted into the outbox (stamped by
	// the Sender at publish). Distinct from Metadata.Time (the event's
	// occurred-at, which publishers may backdate). Used as the age anchor for
	// drain-lag observability and insert-time retention.
	CreateTime time.Time
}
