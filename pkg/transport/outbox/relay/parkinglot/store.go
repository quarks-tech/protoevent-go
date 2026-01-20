package parkinglot

import (
	"context"
	"time"

	"github.com/quarks-tech/protoevent-go/pkg/transport/outbox"
	"github.com/quarks-tech/protoevent-go/pkg/transport/outbox/relay"
)

// Store defines operations for parking lot functionality with wait queue support.
type Store interface {
	relay.Store

	// MoveOutboxMessageToWait moves a message from outbox_messages to outbox_messages_wait.
	// The message will be available for retry after the specified wait duration.
	MoveOutboxMessageToWait(ctx context.Context, id string, retryTime time.Time) error

	// MoveWaitingMessagesToOutbox moves messages from outbox_messages_wait back to outbox_messages
	// where retry_time <= now. Returns the number of messages moved.
	MoveWaitingMessagesToOutbox(ctx context.Context) (int, error)

	// MoveOutboxMessageToParkingLot moves a message from outbox_messages to outbox_messages_pl.
	// Used when max retries exceeded.
	MoveOutboxMessageToParkingLot(ctx context.Context, id, reason string) error

	// GetOutboxMessageRetryCount returns the current retry count for a message.
	GetOutboxMessageRetryCount(ctx context.Context, id string) (int, error)

	// IncrementOutboxMessageRetryCount increments the retry count in message metadata.
	// Returns the new retry count.
	IncrementOutboxMessageRetryCount(ctx context.Context, id string) (int, error)
}

// LeaderStore combines Store with leader election support.
type LeaderStore interface {
	Store
	relay.LeaderStore
}

// PartitionedStore combines Store with partition truncation support.
type PartitionedStore interface {
	Store
	relay.PartitionedStore
}

// Message re-exports outbox.Message for convenience.
type Message = outbox.Message
