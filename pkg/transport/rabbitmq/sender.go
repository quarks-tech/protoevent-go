package rabbitmq

import (
	"context"
	"fmt"
	"strings"

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

type Sender struct {
	client  *amqpx.Client
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
		return conn.Channel().ExchangeDeclare(desc.ServiceName, amqp.ExchangeTopic, true, false, false, false, nil)
	})
}

func (s *Sender) Send(ctx context.Context, meta *event.Metadata, data []byte) error {
	mess, err := s.options.marshaler.Marshal(meta, data)
	if err != nil {
		return fmt.Errorf("marshal to rabbitmq message: %w", err)
	}

	mess.DeliveryMode = s.options.deliveryMode

	pos := strings.LastIndex(meta.Type, ".")
	exchange := mess.Type[:pos]
	routingKey := mess.Type[pos+1:]

	return s.client.Process(ctx, func(ctx context.Context, conn *connpool.Conn) error {
		return conn.Channel().Publish(exchange, routingKey, false, false, mess)
	})
}
