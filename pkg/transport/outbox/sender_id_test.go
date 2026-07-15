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

func TestSenderDefaultReusesMetadataID(t *testing.T) {
	store := &captureStore{}
	sender := outbox.NewSender(store)

	if err := sender.Send(context.Background(), newMetadata("event-id"), nil); err != nil {
		t.Fatalf("send: %v", err)
	}

	// Default reuses the metadata ID: the relay persists this as event_id and
	// reconstructs the emitted CloudEvents Metadata.ID from it, so the row and
	// the event must share one identity end to end.
	if store.msg.ID != "event-id" {
		t.Fatalf("default message ID = %q, want event-id (reused from metadata)", store.msg.ID)
	}
}

func TestSenderGenerateV4Option(t *testing.T) {
	store := &captureStore{}
	sender := outbox.NewSender(store, outbox.WithRowIDGenerator(outbox.GenerateUUIDv4))

	if err := sender.Send(context.Background(), newMetadata("event-id"), nil); err != nil {
		t.Fatalf("send: %v", err)
	}

	id, err := uuid.Parse(store.msg.ID)
	if err != nil {
		t.Fatalf("GenerateUUIDv4 message ID is not a valid UUID: %v", err)
	}
	if id.Version() != 4 {
		t.Fatalf("GenerateUUIDv4 message ID version = %d, want 4", id.Version())
	}
	if store.msg.ID == "event-id" {
		t.Fatal("GenerateUUIDv4 reused metadata ID, want freshly minted")
	}
}

func TestSenderReuseMetadataID(t *testing.T) {
	store := &captureStore{}
	sender := outbox.NewSender(store, outbox.WithRowIDGenerator(outbox.ReuseMetadataID))

	if err := sender.Send(context.Background(), newMetadata("event-id"), nil); err != nil {
		t.Fatalf("send: %v", err)
	}

	if store.msg.ID != "event-id" {
		t.Fatalf("message ID = %q, want event-id (reused from metadata)", store.msg.ID)
	}
}

func TestSenderCustomGenerator(t *testing.T) {
	store := &captureStore{}
	sender := outbox.NewSender(store, outbox.WithRowIDGenerator(func(_ *event.Metadata) (string, error) {
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
	sender := outbox.NewSender(store, outbox.WithRowIDGenerator(func(_ *event.Metadata) (string, error) {
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
