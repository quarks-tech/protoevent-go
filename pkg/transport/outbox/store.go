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
// Methods are prefixed with "Outbox" to avoid conflicts with application store methods.
type RelayStore interface {
	// GetOutboxCursor returns the stored cursor position, or the ID of the first pending message
	// if no cursor is stored. Returns empty string if there are no pending messages.
	GetOutboxCursor(ctx context.Context) (string, error)

	// SaveOutboxCursor persists the current cursor position for resuming after restarts.
	SaveOutboxCursor(ctx context.Context, cursor string) error

	// ListPendingOutboxMessages retrieves messages that haven't been sent yet,
	// starting from the given cursor ID (or from the beginning if cursor is empty),
	// ordered by id (FIFO). Returns up to 'limit' messages.
	// Uses UUID v7 IDs which are time-sortable.
	ListPendingOutboxMessages(ctx context.Context, cursor string, limit int) ([]*Message, error)

	// UpdateOutboxMessagesSentTime updates the sent_time timestamp for the given message IDs.
	// Used when ProcessingModeMarkSent is configured.
	UpdateOutboxMessagesSentTime(ctx context.Context, t time.Time, ids ...string) error

	// DeleteOutboxMessages removes the messages with given IDs from the outbox table.
	// Used when ProcessingModeDelete is configured.
	DeleteOutboxMessages(ctx context.Context, ids ...string) error
}

// PartitionedRelayStore is an optional interface for stores that support hourly partitioning.
// Implement this interface to enable automatic partition truncation with WithRetentionHours.
type PartitionedRelayStore interface {
	// TruncateOutboxPartitions truncates the specified hourly partitions (p0-p23).
	// Used for fast cleanup when using HASH(HOUR(create_time)) partitioning.
	TruncateOutboxPartitions(ctx context.Context, partitions ...int) error
}
