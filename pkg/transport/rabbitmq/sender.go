package rabbitmq

import (
	"context"
	"fmt"

	amqp "github.com/rabbitmq/amqp091-go"

	"github.com/quarks-tech/amqpx"
	"github.com/quarks-tech/amqpx/connpool"

	"github.com/quarks-tech/protoevent-go/pkg/event"
	"github.com/quarks-tech/protoevent-go/pkg/eventbus"
	"github.com/quarks-tech/protoevent-go/pkg/transport/rabbitmq/message"
)

const (
	DeliveryModeTransient  = 1
	DeliveryModePersistent = 2
)

func WithTransientDeliveryMode() SenderOption {
	return func(opts *senderOptions) {
		opts.deliveryMode = DeliveryModeTransient
	}
}

func WithMessageMarshaler(m Marshaler) SenderOption {
	return func(opts *senderOptions) {
		opts.marshaler = m
	}
}

type senderOptions struct {
	deliveryMode uint8
	marshaler    Marshaler
}

func defaultSenderOptions() senderOptions {
	return senderOptions{
		marshaler:    message.Marshaler{},
		deliveryMode: DeliveryModePersistent,
	}
}

type SenderOption func(opts *senderOptions)

type commandProcessor interface {
	Process(context.Context, amqpx.Command) error
}

type Sender struct {
	client  commandProcessor
	options senderOptions
}

func NewSender(client *amqpx.Client, opts ...SenderOption) *Sender {
	options := defaultSenderOptions()

	for _, opt := range opts {
		opt(&options)
	}

	return &Sender{
		client:  client,
		options: options,
	}
}

func (s *Sender) Setup(ctx context.Context, desc *eventbus.ServiceDesc) error {
	return s.client.Process(ctx, func(ctx context.Context, conn *connpool.Conn) error {
		if err := conn.Channel().ExchangeDeclare(desc.ServiceName, amqp.ExchangeTopic, true, false, false, false, nil); err != nil {
			return fmt.Errorf("declare exchange %q: %w", desc.ServiceName, err)
		}

		return nil
	})
}

// splitEventType maps an event type onto AMQP routing: the service part is the
// exchange, the event part the routing key. The split itself is event.SplitType —
// the one definition of the event-type shape, shared with the publisher's
// write-side guard and the subscriber's routing, so the three cannot drift.
func splitEventType(eventType string) (exchange, routingKey string, err error) {
	return event.SplitType(eventType)
}

func (s *Sender) Send(ctx context.Context, meta *event.Metadata, data []byte) error {
	mess, err := s.options.marshaler.Marshal(meta, data)
	if err != nil {
		return fmt.Errorf("marshal to rabbitmq message: %w", err)
	}

	mess.DeliveryMode = s.options.deliveryMode

	// Route off meta.Type, the authoritative event type — a marshaler is free to
	// leave amqp.Publishing.Type empty or rewrite it. meta.Type may come from a
	// persisted outbox row, so a malformed value must error, not panic.
	exchange, routingKey, err := splitEventType(meta.Type)
	if err != nil {
		return err
	}

	return s.client.Process(ctx, func(ctx context.Context, conn *connpool.Conn) error {
		if err := conn.Channel().PublishWithContext(ctx, exchange, routingKey, false, false, mess); err != nil {
			return fmt.Errorf("publish to exchange %q: %w", exchange, err)
		}

		return nil
	})
}
