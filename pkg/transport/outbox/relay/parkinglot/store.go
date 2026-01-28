package parkinglot

import (
	"context"
	"time"

	"github.com/quarks-tech/protoevent-go/pkg/transport/outbox"
	"github.com/quarks-tech/protoevent-go/pkg/transport/outbox/relay"
)

// Store defines operations for the parking lot relay with four tables:
//   - outbox_pending: messages waiting to be sent
//   - outbox_wait: messages in retry backoff
//   - outbox_parking_lot: permanently failed messages
//   - outbox_completed: successfully sent messages (optional, for audit)
//
// This eliminates cursor management and prevents race conditions with UUID v7 ordering.
//
// Note: Method signatures are intentionally consistent with relay.Store for batch operations.
// The retry count is obtained from Message.RetryCount (populated by ListPendingMessages).
type Store interface {
	// ListPendingMessages retrieves messages from the pending table,
	// ordered by id (FIFO). Returns up to 'limit' messages.
	// Must populate Message.RetryCount for retry tracking.
	ListPendingMessages(ctx context.Context, limit int) ([]*outbox.Message, error)

	// DeletePendingMessages removes messages from the pending table.
	// Called after messages are successfully sent (when no audit trail is needed).
	DeletePendingMessages(ctx context.Context, ids ...string) error

	// MovePendingToCompleted moves messages from pending to completed table.
	// Used when audit trail is needed. The completed table stores sent_time.
	MovePendingToCompleted(ctx context.Context, sentTime time.Time, ids ...string) error

	// MovePendingToWait moves a message from pending to wait table for retry.
	// The message will be moved back to pending after retryTime passes.
	// The retryCount should be stored in the wait table for tracking.
	MovePendingToWait(ctx context.Context, id string, retryTime time.Time, retryCount int) error

	// MoveWaitToPending moves messages from wait table back to pending table
	// where retry_time <= now.
	MoveWaitToPending(ctx context.Context) error

	// MovePendingToParkingLot moves a message from pending to parking lot table.
	// Used when max retries are exceeded. The reason describes why it was parked.
	MovePendingToParkingLot(ctx context.Context, id, reason string) error
}

// LeaderStore combines Store with leader election support.
type LeaderStore interface {
	Store
	relay.LeaderStore
}
