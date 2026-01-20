package outbox

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/quarks-tech/protoevent-go/pkg/event"
)

// Sender implements eventbus.Sender by persisting events to an outbox store.
// It is designed to be used within a database transaction to ensure
// atomicity between business operations and event publishing.
type Sender struct {
	store Store
}

// NewSender creates a new outbox Sender with the given store.
// The store should be transaction-scoped to ensure atomicity.
func NewSender(store Store) *Sender {
	return &Sender{store: store}
}

// Send persists the event to the outbox store.
// This should be called within the same transaction as business operations.
func (s *Sender) Send(ctx context.Context, metadata *event.Metadata, data []byte) error {
	id, err := uuid.NewV7()
	if err != nil {
		return err
	}

	msg := &Message{
		ID:         id.String(),
		Metadata:   metadata,
		Data:       data,
		CreateTime: time.Now(),
	}

	return s.store.CreateOutboxMessage(ctx, msg)
}
