package outbox

import (
	"context"
)

// Store defines the interface for outbox persistence operations.
// Users must implement this interface with their database layer.
//
// The implementation should use the same database connection/transaction
// as the business logic to ensure atomicity.
type Store interface {
	// CreateOutboxMessage persists an outbox message within the current transaction.
	CreateOutboxMessage(ctx context.Context, msg *Message) error
}
