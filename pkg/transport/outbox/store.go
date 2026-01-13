package outbox

import (
	"context"
	"time"
)

// Store defines the interface for outbox persistence operations.
// Users must implement this interface with their database layer.
//
// The implementation should use the same database connection/transaction
// as the business logic to ensure atomicity.
type Store interface {
	// CreateMessage persists an outbox message within the current transaction.
	CreateMessage(ctx context.Context, msg *Message) error
}

// RelayStore defines operations needed by the message relay.
// This is typically implemented by a non-transactional store instance.
type RelayStore interface {
	// GetCursor returns the ID of the first pending message for cursor initialization.
	// Returns empty string if there are no pending messages.
	GetCursor(ctx context.Context) (string, error)

	// ListPendingMessages retrieves messages that haven't been sent yet,
	// starting from the given cursor ID (or from the beginning if cursor is empty),
	// ordered by id (FIFO). Returns up to 'limit' messages.
	// Uses UUID v7 IDs which are time-sortable.
	ListPendingMessages(ctx context.Context, cursor string, limit int) ([]*Message, error)

	// UpdateMessagesSentTime updates the sent_time timestamp for the given message IDs.
	// Used when ProcessingModeMarkSent is configured.
	UpdateMessagesSentTime(ctx context.Context, t time.Time, ids ...string) error

	// DeleteMessages removes the messages with given IDs from the outbox table.
	// Used when ProcessingModeDelete is configured.
	DeleteMessages(ctx context.Context, ids ...string) error
}
