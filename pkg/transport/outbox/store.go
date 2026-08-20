package outbox

import (
	"context"
	"errors"
)

// ErrAlreadyPublished reports that this event is already durably in the outbox: a row
// with the same event ID exists, so the publish was a no-op rather than a failure.
//
// It exists because the natural reaction to it is the opposite of the natural reaction
// to an error. Retrying business transactions is routine on TiDB — a write conflict, a
// deadlock, a region becoming unavailable, or a commit whose outcome was AMBIGUOUS
// because the client lost the connection after the primary key was written. The retry
// re-runs the handler, which re-publishes the same event under the same ID (the default
// row-ID generator reuses Metadata.ID), and the unique index rejects it. Reported as a
// plain error that aborts the caller's transaction, that fails the user's request
// permanently — every retry hitting the same wall — for an event that is already
// published and will be delivered.
//
// A caller that generates one event ID per business operation should treat this as
// success and commit. A caller that reuses IDs across genuinely different events should
// treat it as the bug it is.
var ErrAlreadyPublished = errors.New("outbox: event already published")

// Store defines the interface for outbox persistence operations.
// Users must implement this interface with their database layer.
//
// The implementation should use the same database connection/transaction
// as the business logic to ensure atomicity.
type Store interface {
	// CreateOutboxMessage persists an outbox message within the current transaction.
	//
	// Implementations MUST report a same-ID row that already exists as
	// ErrAlreadyPublished (wrapped), so a retried business transaction can tell "this
	// event is already durable" from a real write failure. See that sentinel.
	CreateOutboxMessage(ctx context.Context, msg *Message) error
}
