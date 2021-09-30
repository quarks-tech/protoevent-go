package rabbitmq

import (
	"context"
	"strings"
	"time"

	"github.com/quarks-tech/protoevent-go/pkg/event"
	"github.com/quarks-tech/protoevent-go/pkg/transport/rabbitmq/connpool"
	"github.com/streadway/amqp"
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

type senderOptions struct {
	deliveryMode uint8
}

func defaultSenderOptions() senderOptions {
	return senderOptions{
		deliveryMode: DeliveryModePersistent,
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

func (s *Sender) Send(ctx context.Context, meta *event.Metadata, data []byte) error {
	publishing := newPublishing(meta, data)
	publishing.DeliveryMode = s.options.deliveryMode

	pos := strings.LastIndex(publishing.Type, ".")
	exchange := publishing.Type[:pos]
	routingKey := publishing.Type[pos+1:]

	return s.client.Process(ctx, func(ctx context.Context, conn *connpool.Conn) error {
		return conn.Channel().Publish(exchange, routingKey, false, false, publishing)
	})
}

func newPublishing(meta *event.Metadata, data []byte) amqp.Publishing {
	return amqp.Publishing{
		Type:        meta.Type,
		ContentType: meta.DataContentType,
		Headers:     buildPublishingHeaders(meta),
		Body:        data,
	}
}

func buildPublishingHeaders(meta *event.Metadata) amqp.Table {
	return amqp.Table{
		"cloudEvents:time":        meta.Time.Format(time.RFC3339),
		"cloudEvents:id":          meta.ID,
		"cloudEvents:specversion": meta.SpecVersion,
		"cloudEvents:source":      meta.Source,
	}
}
