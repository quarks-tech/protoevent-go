package eventbus_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/quarks-tech/protoevent-go/pkg/event"
	"github.com/quarks-tech/protoevent-go/pkg/eventbus"
)

// captureSender records the metadata of the last sent event.
type captureSender struct {
	md *event.Metadata
}

func (s *captureSender) Send(_ context.Context, md *event.Metadata, _ []byte) error {
	s.md = md
	return nil
}

func TestPublisherDefaultIDIsUUIDv4(t *testing.T) {
	sender := &captureSender{}
	p := eventbus.NewPublisher(sender)

	if err := p.Publish(context.Background(), "test.event", &emptypb.Empty{}); err != nil {
		t.Fatalf("publish: %v", err)
	}

	id, err := uuid.Parse(sender.md.ID)
	if err != nil {
		t.Fatalf("default ID is not a valid UUID: %v", err)
	}
	if id.Version() != 4 {
		t.Fatalf("default ID version = %d, want 4", id.Version())
	}
}

func TestPublisherCallerSuppliedIDWins(t *testing.T) {
	sender := &captureSender{}
	called := false
	p := eventbus.NewPublisher(sender, eventbus.WithIDGenerator(func() (string, error) {
		called = true
		return "generated", nil
	}))

	err := p.Publish(context.Background(), "test.event", &emptypb.Empty{},
		eventbus.WithEventID("caller-supplied"))
	if err != nil {
		t.Fatalf("publish: %v", err)
	}

	if sender.md.ID != "caller-supplied" {
		t.Fatalf("ID = %q, want caller-supplied", sender.md.ID)
	}
	if called {
		t.Fatal("generator was called despite caller-supplied ID")
	}
}

func TestPublisherCustomGeneratorIsUsed(t *testing.T) {
	sender := &captureSender{}
	p := eventbus.NewPublisher(sender, eventbus.WithIDGenerator(func() (string, error) {
		return "custom-id", nil
	}))

	if err := p.Publish(context.Background(), "test.event", &emptypb.Empty{}); err != nil {
		t.Fatalf("publish: %v", err)
	}

	if sender.md.ID != "custom-id" {
		t.Fatalf("ID = %q, want custom-id", sender.md.ID)
	}
}

func TestPublisherGeneratorErrorPropagates(t *testing.T) {
	sentinel := errors.New("boom")
	sender := &captureSender{}
	p := eventbus.NewPublisher(sender, eventbus.WithIDGenerator(func() (string, error) {
		return "", sentinel
	}))

	err := p.Publish(context.Background(), "test.event", &emptypb.Empty{})
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want wrapped %v", err, sentinel)
	}
}
