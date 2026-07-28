package rabbitmq

import (
	"context"
	"errors"
	"testing"

	amqp "github.com/rabbitmq/amqp091-go"

	"github.com/quarks-tech/amqpx"
	"github.com/quarks-tech/amqpx/connpool"

	"github.com/quarks-tech/protoevent-go/pkg/event"
)

type commandProcessorFunc func(context.Context, amqpx.Command) error

func (f commandProcessorFunc) Process(ctx context.Context, command amqpx.Command) error {
	return f(ctx, command)
}

func TestSenderSendPassesCommandContextToPublish(t *testing.T) {
	commandCtx, cancel := context.WithCancel(t.Context())
	cancel()

	client := commandProcessorFunc(func(_ context.Context, command amqpx.Command) error {
		conn := connpool.NewConn(nil, &amqp.Channel{})

		return command(commandCtx, conn)
	})
	sender := &Sender{
		client:  client,
		options: defaultSenderOptions(),
	}

	err := sender.Send(t.Context(), event.NewMetadata("books.v1.BookCreated"), []byte("event"))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Send() error = %v, want context.Canceled", err)
	}
}

// TestSenderSendRejectsMalformedEventType pins that a malformed event type is an
// ERROR, not a panic. meta.Type can arrive from a persisted outbox row, and the
// relay calls Send from its single Run goroutine: a panic there kills the relay
// process, and because the offset is never committed past the row, every restart
// re-reads it and re-panics — a permanent crash loop no PoisonHandler can
// intercept (it is not a DecodeError).
func TestSenderSendRejectsMalformedEventType(t *testing.T) {
	tests := map[string]string{
		"no dot":            "BookCreated",
		"empty":             "",
		"leading dot":       ".BookCreated",
		"trailing dot":      "books.v1.",
		"only a dot":        ".",
		"no event name":     "books.",
		"no service prefix": ".v1",
	}

	for name, eventType := range tests {
		t.Run(name, func(t *testing.T) {
			client := commandProcessorFunc(func(_ context.Context, _ amqpx.Command) error {
				t.Fatal("Send() reached the broker with a malformed event type")

				return nil
			})
			sender := &Sender{client: client, options: defaultSenderOptions()}

			meta := event.NewMetadata("books.v1.BookCreated")
			meta.Type = eventType

			err := sender.Send(t.Context(), meta, []byte("event"))
			if err == nil {
				t.Fatalf("Send(%q) error = nil, want a malformed-event-type error", eventType)
			}
		})
	}
}

// TestSplitEventTypeIgnoresMarshalerType pins that routing is derived from
// meta.Type, the authoritative event type, and not from amqp.Publishing.Type: a
// custom WithMessageMarshaler may leave the latter empty or rewrite it, and
// indexing one string with a position computed from the other panics or silently
// routes to the wrong exchange.
func TestSplitEventTypeIgnoresMarshalerType(t *testing.T) {
	exchange, routingKey, err := splitEventType("books.v1.BookCreated")
	if err != nil {
		t.Fatalf("splitEventType: %v", err)
	}
	if exchange != "books.v1" || routingKey != "BookCreated" {
		t.Fatalf("splitEventType = (%q, %q), want (\"books.v1\", \"BookCreated\")", exchange, routingKey)
	}
}
