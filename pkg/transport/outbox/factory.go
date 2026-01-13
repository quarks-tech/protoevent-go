package outbox

import (
	"github.com/quarks-tech/protoevent-go/pkg/eventbus"
)

// PublisherFactory creates eventbus.Publisher instances bound to a specific store.
// This is used to create transaction-scoped publishers within WithTransaction callbacks.
//
// Example usage:
//
//	type MyStore interface {
//	    outbox.Store
//	    // ... other methods
//	}
//
//	factory := outbox.NewPublisherFactory(publisherOpts...)
//
//	err := txStore.WithTransaction(ctx, func(ctx context.Context, store MyStore) error {
//	    // Business logic...
//
//	    publisher := factory.Create(store)
//	    return publisher.Publish(ctx, event, opts...)
//	})
type PublisherFactory struct {
	options []eventbus.PublisherOption
}

// NewPublisherFactory creates a new factory with the given publisher options.
// These options will be applied to all publishers created by this factory.
func NewPublisherFactory(opts ...eventbus.PublisherOption) *PublisherFactory {
	return &PublisherFactory{options: opts}
}

// Create returns a new eventbus.Publisher that uses the provided store
// for persisting events to the outbox.
//
// The store should be transaction-scoped (e.g., passed into WithTransaction callback)
// to ensure the event is saved atomically with business operations.
func (f *PublisherFactory) Create(store Store) *eventbus.PublisherImpl {
	sender := NewSender(store)
	return eventbus.NewPublisher(sender, f.options...)
}
