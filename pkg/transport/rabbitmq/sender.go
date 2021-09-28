package rabbitmq

import (
	"context"
	"time"

	"github.com/quarks-tech/protoevent-go/pkg/event"
	"github.com/quarks-tech/protoevent-go/pkg/transport/rabbitmq/connpool"
	"github.com/streadway/amqp"
)

const (
	DeliveryModeTransient  = 1
	DeliveryModePersistent = 2
)

type SendOption func(mess *amqp.Publishing)

func WithDeliveryMode(mode uint8) SendOption {
	return func(mess *amqp.Publishing) {
		mess.DeliveryMode = mode
	}
}

type Sender struct {
	client  *Client
	options []SendOption
}

func NewSender(client *Client, options ...SendOption) *Sender {
	return &Sender{
		client:  client,
		options: options,
	}
}

func (s *Sender) Send(ctx context.Context, meta *event.Metadata, data []byte) error {
	publishing := newPublishing(meta, data)

	for _, option := range s.options {
		option(&publishing)
	}

	//@todo exchange, key?
	return s.client.Process(ctx, func(ctx context.Context, conn *connpool.Conn) error {
		return conn.Channel().Publish("example.books.v1", meta.Type, false, false, publishing)
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
