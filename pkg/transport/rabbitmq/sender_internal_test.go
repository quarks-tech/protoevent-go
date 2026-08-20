package rabbitmq

import (
	"context"
	"errors"
	"testing"

	amqp "github.com/rabbitmq/amqp091-go"

	"github.com/quarks-tech/amqpx"
	"github.com/quarks-tech/amqpx/connpool"

	"github.com/quarks-tech/protoevent-go/pkg/event"
	"github.com/quarks-tech/protoevent-go/pkg/transport/rabbitmq/internal/publish"
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
	// Confirms off: the confirm-mode handshake needs a live channel, and what this
	// pins is that the COMMAND's context — not Send's own — reaches the publish.
	options := defaultSenderOptions()
	WithoutPublisherConfirms()(&options)
	sender := &Sender{
		client:   client,
		options:  options,
		confirms: publish.NewConfirms(),
	}

	err := sender.Send(t.Context(), event.NewMetadata("books.v1.BookCreated"), []byte("event"))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Send() error = %v, want context.Canceled", err)
	}
}

// TestSenderConfirmsAreOnByDefault pins the default that makes the outbox's
// at-least-once guarantee real.
//
// Without confirms, Send returns nil as soon as the frame is written, so a broker
// rejection — the 404 for an exchange that does not exist yet, a resource alarm, a
// failed persist — arrives asynchronously, after Send already reported success. An
// outbox relay takes that answer as authorization to commit its offset (or persist
// its resume token) past the event, so the event is dropped and later swept while
// Observer.OnDrained counts it as sent: silent loss, in the one component whose
// entire purpose is not losing anything.
func TestSenderConfirmsAreOnByDefault(t *testing.T) {
	if !defaultSenderOptions().confirms {
		t.Fatal("publisher confirms are off by default; an unconfirmed publish lets the relay commit past discarded events")
	}

	options := defaultSenderOptions()
	WithoutPublisherConfirms()(&options)
	if options.confirms {
		t.Fatal("WithoutPublisherConfirms() did not turn confirms off")
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

// retypingMarshaler rewrites amqp.Publishing.Type, the thing routing must NOT be
// derived from.
type retypingMarshaler struct {
	publishingType string
}

func (m retypingMarshaler) Marshal(md *event.Metadata, data []byte) (amqp.Publishing, error) {
	return amqp.Publishing{Type: m.publishingType, ContentType: md.DataContentType, Body: data}, nil
}

func (m retypingMarshaler) Unmarshal(*amqp.Delivery) (*event.Metadata, []byte, error) {
	return nil, nil, errors.New("not used")
}

// TestSendRoutesOnMetadataTypeNotMarshalerType pins that routing is derived from
// meta.Type, the authoritative event type, and not from amqp.Publishing.Type: a
// custom WithMessageMarshaler may leave the latter empty or rewrite it, and
// indexing one string with a position computed from the other panics or silently
// routes to the wrong exchange.
//
// A well-formed meta.Type must therefore reach the broker even when the marshaler
// hands back a Publishing.Type that would not survive the split — here the command
// context is pre-canceled, so success is "got as far as the publish".
func TestSendRoutesOnMetadataTypeNotMarshalerType(t *testing.T) {
	for name, publishingType := range map[string]string{
		"empty":     "",
		"malformed": "BookCreated",
	} {
		t.Run(name, func(t *testing.T) {
			commandCtx, cancel := context.WithCancel(t.Context())
			cancel()

			client := commandProcessorFunc(func(_ context.Context, command amqpx.Command) error {
				return command(commandCtx, connpool.NewConn(nil, &amqp.Channel{}))
			})

			options := defaultSenderOptions()
			WithoutPublisherConfirms()(&options)
			WithMessageMarshaler(retypingMarshaler{publishingType: publishingType})(&options)
			sender := &Sender{
				client:   client,
				options:  options,
				confirms: publish.NewConfirms(),
			}

			err := sender.Send(t.Context(), event.NewMetadata("books.v1.BookCreated"), []byte("event"))
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("Send() error = %v, want context.Canceled (routing must come from meta.Type)", err)
			}
		})
	}
}
