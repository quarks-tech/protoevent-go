package outbox_test

import (
	"context"
	"testing"

	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/quarks-tech/protoevent-go/pkg/eventbus"
	"github.com/quarks-tech/protoevent-go/pkg/transport/outbox"
)

// TestFactoryForwardsSenderOptions verifies that sender options supplied to
// the factory reach the underlying outbox sender. GenerateUUIDv4 is the
// NON-default row-ID generator on purpose: with the default (ReuseMetadataID,
// row ID == metadata ID) the assertion would pass even if the option were
// silently dropped.
func TestFactoryForwardsSenderOptions(t *testing.T) {
	identity := func(p eventbus.Publisher) eventbus.Publisher { return p }

	factory := outbox.NewPublisherFactory(identity,
		outbox.WithSenderOptions(outbox.WithRowIDGenerator(outbox.GenerateUUIDv4)),
		outbox.WithPublisherOptions(
			eventbus.WithDefaultPublishOptions(eventbus.WithEventSource("books-service")),
		),
	)

	store := &captureStore{}
	publisher := factory.Create(store)

	if err := publisher.Publish(context.Background(), "test.event", &emptypb.Empty{}); err != nil {
		t.Fatalf("publish: %v", err)
	}

	// GenerateUUIDv4 (the forwarded sender option) mints a fresh row ID, so
	// it must differ from the event's metadata ID — the default would match.
	if store.msg.ID == store.msg.Metadata.ID {
		t.Fatalf("message ID %q == metadata ID %q; WithRowIDGenerator not forwarded (default applied)",
			store.msg.ID, store.msg.Metadata.ID)
	}
	// The publish option was forwarded too.
	if store.msg.Metadata.Source != "books-service" {
		t.Fatalf("metadata source = %q, want books-service", store.msg.Metadata.Source)
	}
}
