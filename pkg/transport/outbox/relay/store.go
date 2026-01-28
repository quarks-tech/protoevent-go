package relay

import (
	"context"
	"time"

	"github.com/quarks-tech/protoevent-go/pkg/transport/outbox"
)

// Store defines operations needed by the message relay.
// This is typically implemented by a non-transactional store instance.
//
// The relay uses a two-table approach:
//   - outbox_pending: messages waiting to be sent
//   - outbox_completed: messages that have been sent (optional, for audit)
//
// This eliminates the need for cursor management and prevents race conditions
// where messages with earlier UUIDs are inserted after later ones.
type Store interface {
	// ListPendingMessages retrieves messages from the pending table,
	// ordered by id (FIFO). Returns up to 'limit' messages.
	// Uses UUID v7 IDs which are time-sortable.
	ListPendingMessages(ctx context.Context, limit int) ([]*outbox.Message, error)

	// DeletePendingMessages removes the messages with given IDs from the pending table.
	// Called after messages are successfully sent.
	DeletePendingMessages(ctx context.Context, ids ...string) error

	// MovePendingToCompleted moves messages from pending to completed table.
	// Used when audit trail is needed. The completed table stores sent_time.
	// Optional: implementations may just delete if no audit is needed.
	MovePendingToCompleted(ctx context.Context, sentTime time.Time, ids ...string) error
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
