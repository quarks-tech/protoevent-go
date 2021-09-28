package rabbitmq

import (
	"context"
	"os"
	"time"

	"github.com/quarks-tech/protoevent-go/pkg/event"
	"github.com/quarks-tech/protoevent-go/pkg/eventbus"
	"github.com/quarks-tech/protoevent-go/pkg/transport/rabbitmq/connpool"
	"github.com/streadway/amqp"
	"golang.org/x/sync/errgroup"
)

const dlxSuffix = "-dlx"

type ReceiverOption func(s *Receiver)

func WithWorkerNum(c int) ReceiverOption {
	return func(s *Receiver) {
		s.workerCount = c
	}
}

func WithPrefetchCount(c int) ReceiverOption {
	return func(s *Receiver) {
		s.prefetchCount = c
	}
}

func WithAutogen() ReceiverOption {
	return func(s *Receiver) {
		name, err := os.Hostname()
		if err != nil {
			name = ""
		}

		s.consumerName = name
	}
}

type Receiver struct {
	client        *Client
	consumerName  string
	consumerTag   string
	queue         string
	prefetchCount int
	workerCount   int
	enableDLX     bool
}

func NewReceiver(client *Client, opts ...ReceiverOption) *Receiver {
	s := &Receiver{
		client:        client,
		prefetchCount: 1,
		enableDLX:     true,
		queue:         "books",
		consumerTag:   "test",
	}

	for _, opt := range opts {
		opt(s)
	}

	if s.queue == "" {
		s.queue = s.consumerName
	}

	return s
}

func (r *Receiver) Setup(ctx context.Context, infos ...eventbus.ServiceInfo) error {
	return r.client.Process(ctx, func(ctx context.Context, conn *connpool.Conn) error {
		return r.setup(conn, infos)
	})
}

func (r *Receiver) setup(conn *connpool.Conn, infos []eventbus.ServiceInfo) error {
	var queueDeclareArgs amqp.Table

	if r.enableDLX {
		dlxExchange := r.queue + dlxSuffix
		dlxQueue := r.queue + dlxSuffix

		queueDeclareArgs = amqp.Table{
			"x-dead-letter-exchange": dlxExchange,
		}

		err := conn.Channel().ExchangeDeclare(dlxExchange, amqp.ExchangeFanout, true, false, false, false, nil)
		if err != nil {
			return err
		}

		_, err = conn.Channel().QueueDeclare(dlxQueue, true, false, false, false, nil)
		if err != nil {
			return err
		}

		if err = conn.Channel().QueueBind(dlxQueue, "", dlxExchange, false, nil); err != nil {
			return err
		}
	}

	_, err := conn.Channel().QueueDeclare(r.queue, true, false, false, false, queueDeclareArgs)
	if err != nil {
		return err
	}

	for _, info := range infos {
		for _, eventName := range info.Events {
			fullName := info.ServiceName + "." + eventName

			if err = conn.Channel().QueueBind(r.queue, fullName, info.ServiceName, false, nil); err != nil {
				return err
			}
		}
	}

	return nil
}

func (r *Receiver) Receive(ctx context.Context, fn func(md *event.Metadata, data []byte) error) error {
	return r.client.Process(ctx, func(ctx context.Context, conn *connpool.Conn) error {
		return r.receive(conn, ctx, fn)
	})
}

func (r *Receiver) receive(conn *connpool.Conn, ctx context.Context, fn func(md *event.Metadata, data []byte) error) error {
	if err := conn.Channel().Qos(r.prefetchCount, 0, false); err != nil {
		return err
	}

	deliveries, err := conn.Channel().Consume(r.queue, r.consumerTag, false, false, false, false, nil)
	if err != nil {
		return err
	}

	eg, egCtx := errgroup.WithContext(context.Background())

	eg.Go(func() error {
		select {
		case <-ctx.Done():
			return conn.Channel().Cancel(r.consumerTag, false)
		case <-egCtx.Done():
			return conn.Close()
		case connErr := <-conn.NotifyClose(make(chan *amqp.Error)):
			return connErr
		}
	})

	for i := 0; i < r.workerCount; i++ {
		eg.Go(func() error {
			for delivery := range deliveries {
				select {
				case <-ctx.Done():
					return nil
				default:
					md, data, err := parseDelivery(&delivery)
					if err == nil {
						err = fn(md, data)
					}

					if ackErr := doAcknowledge(&delivery, err); ackErr != nil {
						return ackErr
					}
				}
			}

			return nil
		})
	}

	return eg.Wait()
}

func parseDelivery(d *amqp.Delivery) (*event.Metadata, []byte, error) {
	meta := &event.Metadata{
		SpecVersion:     d.Headers["cloudEvents:specversion"].(string),
		DataContentType: d.ContentType,
		Type:            d.Type,
	}

	var err error

	meta.Time, err = time.Parse(time.RFC3339, d.Headers["cloudEvents:time"].(string))
	if err != nil {
		return nil, nil, eventbus.NewUnprocessableEventError(err)
	}

	return meta, d.Body, nil
}

func doAcknowledge(m *amqp.Delivery, err error) error {
	switch {
	case err == nil:
		return m.Ack(false)
	case eventbus.IsUnprocessableEventError(err):
		return m.Reject(false)
	default:
		return m.Reject(true)
	}
}
