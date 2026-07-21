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
