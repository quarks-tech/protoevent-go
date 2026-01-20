package relay

import (
	"context"
	"time"

	"github.com/quarks-tech/protoevent-go/pkg/transport/outbox"
)

// Store defines operations needed by the message relay.
// This is typically implemented by a non-transactional store instance.
type Store interface {
	// GetOutboxCursor returns the stored cursor position, or the ID of the first pending message
	// if no cursor is stored. Returns empty string if there are no pending messages.
	GetOutboxCursor(ctx context.Context) (string, error)

	// SaveOutboxCursor persists the current cursor position for resuming after restarts.
	SaveOutboxCursor(ctx context.Context, cursor string) error

	// ListPendingOutboxMessages retrieves messages that haven't been sent yet,
	// starting from the given cursor ID (or from the beginning if cursor is empty),
	// ordered by id (FIFO). Returns up to 'limit' messages.
	// Uses UUID v7 IDs which are time-sortable.
	ListPendingOutboxMessages(ctx context.Context, cursor string, limit int) ([]*outbox.Message, error)

	// UpdateOutboxMessagesSentTime updates the sent_time timestamp for the given message IDs.
	// Used when ProcessingModeMarkSent is configured.
	UpdateOutboxMessagesSentTime(ctx context.Context, t time.Time, ids ...string) error

	// DeleteOutboxMessages removes the messages with given IDs from the outbox table.
	// Used when ProcessingModeDelete is configured.
	DeleteOutboxMessages(ctx context.Context, ids ...string) error
}

// PartitionedStore is an optional interface for stores that support hourly partitioning.
// Implement this interface to enable automatic partition truncation with WithRetentionHours.
type PartitionedStore interface {
	// TruncateOutboxPartitions truncates the specified hourly partitions (p0-p23).
	// Used for fast cleanup when using HASH(HOUR(create_time)) partitioning.
	TruncateOutboxPartitions(ctx context.Context, partitions ...int) error
}

// LeaderStore is an optional interface for stores that support leader election.
// Implement this interface to enable running multiple relay instances with automatic failover.
// Only one instance will be active at a time, ensuring strict FIFO ordering.
type LeaderStore interface {
	// TryAcquireLeaderLock attempts to acquire or renew the leader lock.
	// Returns true if this instance (identified by holderID) holds the lock after the call.
	// The lock expires after leaseTTL if not renewed.
	TryAcquireLeaderLock(ctx context.Context, name, holderID string, leaseTTL time.Duration) (bool, error)
}
