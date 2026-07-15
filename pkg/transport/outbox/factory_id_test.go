package outbox_test

import (
	"context"
	"testing"

	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/quarks-tech/protoevent-go/pkg/eventbus"
	"github.com/quarks-tech/protoevent-go/pkg/transport/outbox"
)

// TestFactoryForwardsSenderOptions verifies that sender options supplied to the
// factory reach the underlying outbox sender — here, ReuseMetadataID makes the
// persisted Message.ID equal the event's Metadata.ID.
func TestFactoryForwardsSenderOptions(t *testing.T) {
	identity := func(p eventbus.Publisher) eventbus.Publisher { return p }

	factory := outbox.NewPublisherFactory(identity,
		outbox.WithSenderOptions(outbox.WithRowIDGenerator(outbox.ReuseMetadataID)),
		outbox.WithPublisherOptions(
			eventbus.WithDefaultPublishOptions(eventbus.WithEventSource("books-service")),
		),
	)

	store := &captureStore{}
	publisher := factory.Create(store)

	if err := publisher.Publish(context.Background(), "test.event", &emptypb.Empty{}); err != nil {
		t.Fatalf("publish: %v", err)
	}

	// ReuseMetadataID (a forwarded sender option) makes the row ID equal the
	// event's metadata ID, whatever the publisher minted.
	if store.msg.ID != store.msg.Metadata.ID {
		t.Fatalf("message ID %q != metadata ID %q; sender option not forwarded",
			store.msg.ID, store.msg.Metadata.ID)
	}
	// The publish option was forwarded too.
	if store.msg.Metadata.Source != "books-service" {
		t.Fatalf("metadata source = %q, want books-service", store.msg.Metadata.Source)
	}
}
