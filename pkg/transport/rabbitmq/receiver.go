package rabbitmq

import (
	"context"
	"runtime"
	"time"

	"github.com/google/uuid"
	"github.com/quarks-tech/protoevent-go/pkg/event"
	"github.com/quarks-tech/protoevent-go/pkg/eventbus"
	"github.com/quarks-tech/protoevent-go/pkg/transport/rabbitmq/connpool"
	"github.com/streadway/amqp"
	"golang.org/x/sync/errgroup"
)

const dlxSuffix = "-dlx"

type receiverOptions struct {
	workerCount   int
	prefetchCount int
	consumerName  string
	consumerTag   string
	enableDLX     bool
}

func defaultReceiverOptions() receiverOptions {
	maxProcs := runtime.GOMAXPROCS(0)

	return receiverOptions{
		workerCount:   maxProcs,
		prefetchCount: maxProcs * 3,
	}
}

func (o *receiverOptions) complete() {
	if o.consumerName == "" {
		o.consumerName = "protoevent-go"
	}

	if o.consumerTag == "" {
		o.consumerTag = o.consumerName + "-" + uuid.New().String()
	}
}

type ReceiverOption func(o *receiverOptions)

func WithWorkerNum(c int) ReceiverOption {
	return func(o *receiverOptions) {
		o.workerCount = c
	}
}

func WithDLX() ReceiverOption {
	return func(o *receiverOptions) {
		o.enableDLX = true
	}
}

func WithPrefetchCount(c int) ReceiverOption {
	return func(o *receiverOptions) {
		o.prefetchCount = c
	}
}

type Receiver struct {
	client  *Client
	options receiverOptions
	queue   string
}

func NewReceiver(client *Client, queue string, opts ...ReceiverOption) *Receiver {
	options := defaultReceiverOptions()

	for _, opt := range opts {
		opt(&options)
	}

	options.complete()

	s := &Receiver{
		client:  client,
		options: options,
		queue:   queue,
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

	if r.options.enableDLX {
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
			if err = conn.Channel().QueueBind(r.queue, eventName, info.ServiceName, false, nil); err != nil {
				return err
			}
		}
	}

	return nil
}

func (r *Receiver) Receive(ctx context.Context, processor eventbus.Processor) error {
	return r.client.Process(ctx, func(ctx context.Context, conn *connpool.Conn) error {
		return r.receive(conn, ctx, processor)
	})
}

func (r *Receiver) receive(conn *connpool.Conn, ctx context.Context, processor eventbus.Processor) error {
	if err := conn.Channel().Qos(r.options.prefetchCount, 0, false); err != nil {
		return err
	}

	deliveries, err := conn.Channel().Consume(r.queue, r.options.consumerTag, false, false, false, false, nil)
	if err != nil {
		return err
	}

	eg, egCtx := errgroup.WithContext(context.Background())

	eg.Go(func() error {
		select {
		case <-ctx.Done():
			return conn.Channel().Cancel(r.options.consumerTag, false)
		case <-egCtx.Done():
			return conn.Close()
		case connErr := <-conn.NotifyClose(make(chan *amqp.Error)):
			return connErr
		}
	})

	for i := 0; i < r.options.workerCount; i++ {
		eg.Go(func() error {
			for delivery := range deliveries {
				select {
				case <-ctx.Done():
					return nil
				default:
					md, data, err := parseDelivery(&delivery)
					if err == nil {
						err = processor(md, data)
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
