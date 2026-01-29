package outbox

import (
	"time"

	"github.com/quarks-tech/protoevent-go/pkg/event"
)

// Message represents an event stored in the outbox table.
type Message struct {
	// ID is the unique identifier of the outbox message.
	ID string

	// Metadata contains CloudEvents metadata.
	Metadata *event.Metadata

	// Data is the serialized event payload.
	Data []byte

	// CreateTime is when the message was created.
	CreateTime time.Time

	// SentTime is when the message was successfully relayed (nil if not sent yet).
	SentTime *time.Time
}
