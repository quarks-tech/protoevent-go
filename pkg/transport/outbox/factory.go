package outbox

import (
	"github.com/quarks-tech/protoevent-go/pkg/eventbus"
)

// factoryOptions holds the configuration accumulated by FactoryOption values.
type factoryOptions struct {
	publisherOptions []eventbus.PublisherOption
	senderOptions    []SenderOption
}

// FactoryOption configures a PublisherFactory.
type FactoryOption func(*factoryOptions)

// WithPublisherOptions applies the given publisher options to every publisher
// the factory creates.
func WithPublisherOptions(opts ...eventbus.PublisherOption) FactoryOption {
	return func(o *factoryOptions) {
		o.publisherOptions = append(o.publisherOptions, opts...)
	}
}

// WithSenderOptions applies the given sender options to the outbox sender behind
// every publisher the factory creates. Use this to reach, for example,
// WithRowIDGenerator(ReuseMetadataID).
func WithSenderOptions(opts ...SenderOption) FactoryOption {
	return func(o *factoryOptions) {
		o.senderOptions = append(o.senderOptions, opts...)
	}
}

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
//	    outbox.WithPublisherOptions(
//	        eventbus.WithDefaultPublishOptions(
//	            eventbus.WithEventSource("books-service"),
//	        ),
//	    ),
//	    outbox.WithSenderOptions(
//	        outbox.WithRowIDGenerator(outbox.ReuseMetadataID),
//	    ),
//	)
//
//	err := txStore.WithTransaction(ctx, func(ctx context.Context, store MyStore) error {
//	    // Business logic...
//	    return factory.Create(store).PublishBookCreatedEvent(ctx, &bookspb.BookCreatedEvent{...})
//	})
type PublisherFactory[T any] struct {
	options factoryOptions
	create  func(eventbus.Publisher) T
}

// NewPublisherFactory creates a new factory with the given typed publisher constructor
// and factory options. The constructor is typically a generated function like
// bookspb.NewEventPublisher.
//
// Use WithPublisherOptions for publisher-level options and WithSenderOptions for
// outbox sender options; both are applied to every publisher the factory creates.
func NewPublisherFactory[T any](create func(eventbus.Publisher) T, opts ...FactoryOption) *PublisherFactory[T] {
	options := factoryOptions{}
	for _, opt := range opts {
		opt(&options)
	}

	return &PublisherFactory[T]{
		options: options,
		create:  create,
	}
}

// Create returns a new typed publisher that uses the provided store
// for persisting events to the outbox.
//
// The store should be transaction-scoped (e.g., passed into WithTransaction callback)
// to ensure the event is saved atomically with business operations.
func (f *PublisherFactory[T]) Create(store Store) T {
	sender := NewSender(store, f.options.senderOptions...)

	return f.create(eventbus.NewPublisher(sender, f.options.publisherOptions...))
}
