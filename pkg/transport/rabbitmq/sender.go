package rabbitmq

import (
	"context"
	"strings"

	"github.com/streadway/amqp"

	"github.com/quarks-tech/protoevent-go/pkg/event"
	"github.com/quarks-tech/protoevent-go/pkg/eventbus"
	"github.com/quarks-tech/protoevent-go/pkg/transport/rabbitmq/connpool"
	"github.com/quarks-tech/protoevent-go/pkg/transport/rabbitmq/message"
	"github.com/quarks-tech/protoevent-go/pkg/transport/rabbitmq/message/cloudevent"
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

func WithMessageFormatter(f message.Formatter) SenderOption {
	return func(opts *senderOptions) {
		opts.messageFormatter = f
	}
}

type senderOptions struct {
	deliveryMode     uint8
	messageFormatter message.Formatter
}

func defaultSenderOptions() senderOptions {
	return senderOptions{
		messageFormatter: cloudevent.Formatter{},
		deliveryMode:     DeliveryModePersistent,
	}
}

type SenderOption func(opts *senderOptions)

type Sender struct {
	client  *Client
	options senderOptions
}

func NewSender(client *Client, opts ...SenderOption) *Sender {
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
	mess := s.options.messageFormatter.Format(meta, data)
	mess.DeliveryMode = s.options.deliveryMode

	pos := strings.LastIndex(meta.Type, ".")
	exchange := mess.Type[:pos]
	routingKey := mess.Type[pos+1:]

	return s.client.Process(ctx, func(ctx context.Context, conn *connpool.Conn) error {
		return conn.Channel().Publish(exchange, routingKey, false, false, mess)
	})
}
