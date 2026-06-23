package outbox_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/quarks-tech/protoevent-go/pkg/event"
	"github.com/quarks-tech/protoevent-go/pkg/transport/outbox"
)

// captureStore records the last message persisted.
type captureStore struct {
	msg *outbox.Message
	err error
}

func (s *captureStore) CreateOutboxMessage(_ context.Context, msg *outbox.Message) error {
	s.msg = msg
	return s.err
}

func newMetadata(id string) *event.Metadata {
	md := event.NewMetadata("test.event")
	md.ID = id
	return md
}

func TestSenderDefaultIDIsUUIDv4(t *testing.T) {
	store := &captureStore{}
	sender := outbox.NewSender(store)

	if err := sender.Send(context.Background(), newMetadata("event-id"), nil); err != nil {
		t.Fatalf("send: %v", err)
	}

	id, err := uuid.Parse(store.msg.ID)
	if err != nil {
		t.Fatalf("default message ID is not a valid UUID: %v", err)
	}
	// v4 (random): the relay orders by create_time, not by row ID, so a
	// time-ordered ID buys nothing — and a monotonic PK hotspots TiDB inserts.
	if id.Version() != 4 {
		t.Fatalf("default message ID version = %d, want 4", id.Version())
	}
	if store.msg.ID == "event-id" {
		t.Fatal("default generator reused metadata ID, want freshly minted")
	}
}

func TestSenderReuseMetadataID(t *testing.T) {
	store := &captureStore{}
	sender := outbox.NewSender(store, outbox.WithIDGenerator(outbox.ReuseMetadataID))

	if err := sender.Send(context.Background(), newMetadata("event-id"), nil); err != nil {
		t.Fatalf("send: %v", err)
	}

	if store.msg.ID != "event-id" {
		t.Fatalf("message ID = %q, want event-id (reused from metadata)", store.msg.ID)
	}
}

func TestSenderCustomGenerator(t *testing.T) {
	store := &captureStore{}
	sender := outbox.NewSender(store, outbox.WithIDGenerator(func(_ *event.Metadata) (string, error) {
		return "custom-id", nil
	}))

	if err := sender.Send(context.Background(), newMetadata("event-id"), nil); err != nil {
		t.Fatalf("send: %v", err)
	}

	if store.msg.ID != "custom-id" {
		t.Fatalf("message ID = %q, want custom-id", store.msg.ID)
	}
}

func TestSenderGeneratorErrorPropagates(t *testing.T) {
	sentinel := errors.New("boom")
	store := &captureStore{}
	sender := outbox.NewSender(store, outbox.WithIDGenerator(func(_ *event.Metadata) (string, error) {
		return "", sentinel
	}))

	err := sender.Send(context.Background(), newMetadata("event-id"), nil)
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want %v", err, sentinel)
	}
	if store.msg != nil {
		t.Fatal("message was persisted despite generator error")
	}
}
