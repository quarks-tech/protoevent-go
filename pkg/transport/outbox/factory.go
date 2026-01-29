package outbox

import (
	"github.com/quarks-tech/protoevent-go/pkg/eventbus"
)

// PublisherFactory creates typed publisher instances bound to a specific store.
// This is used to create transaction-scoped publishers within WithTransaction callbacks.
//
// Example usage:
//
//	type MyStore interface {
//	    outbox.Store
//	    // ... other methods
//	}
//
//	factory := outbox.NewPublisherFactory(bookspb.NewEventPublisher,
//	    eventbus.WithDefaultPublishOptions(
//	        eventbus.WithEventSource("books-service"),
//	    ),
//	)
//
//	err := txStore.WithTransaction(ctx, func(ctx context.Context, store MyStore) error {
//	    // Business logic...
//	    return factory.Create(store).PublishBookCreatedEvent(ctx, &bookspb.BookCreatedEvent{...})
//	})
type PublisherFactory[T any] struct {
	options []eventbus.PublisherOption
	create  func(eventbus.Publisher) T
}

// NewPublisherFactory creates a new factory with the given typed publisher constructor
// and publisher options. The constructor is typically a generated function like
// bookspb.NewEventPublisher.
//
// These options will be applied to all publishers created by this factory.
func NewPublisherFactory[T any](create func(eventbus.Publisher) T, opts ...eventbus.PublisherOption) *PublisherFactory[T] {
	return &PublisherFactory[T]{
		options: opts,
		create:  create,
	}
}

// Create returns a new typed publisher that uses the provided store
// for persisting events to the outbox.
//
// The store should be transaction-scoped (e.g., passed into WithTransaction callback)
// to ensure the event is saved atomically with business operations.
func (f *PublisherFactory[T]) Create(store Store) T {
	return f.create(eventbus.NewPublisher(NewSender(store), f.options...))
}
