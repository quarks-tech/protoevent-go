package parkinglot

import (
	"context"
	"fmt"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
	"github.com/rs/xid"
	"golang.org/x/sync/errgroup"

	"github.com/quarks-tech/amqpx"
	"github.com/quarks-tech/amqpx/connpool"

	"github.com/quarks-tech/protoevent-go/pkg/eventbus"
	"github.com/quarks-tech/protoevent-go/pkg/transport/rabbitmq"
	"github.com/quarks-tech/protoevent-go/pkg/transport/rabbitmq/message"
)

const (
	dlxSuffix        = ".dlx"
	waitSuffix       = ".wait"
	parkingLotSuffix = ".pl"
)

type receiverOptions struct {
	incomingQueue   string
	waitQueue       string
	parkingLotQueue string
	dlxExchange     string
	prefetchCount   int
	consumerTag     string
	setupTopology   bool
	setupBindings   bool
	maxRetries      int64
	minRetryBackoff time.Duration
	marshaler       rabbitmq.Marshaler
	logger          rabbitmq.Logger
}

func defaultReceiverOptions() receiverOptions {
	return receiverOptions{
		marshaler:       message.Marshaler{},
		prefetchCount:   3,
		maxRetries:      3,
		minRetryBackoff: time.Second * 15,
	}
}

type ReceiverOption func(o *receiverOptions)

func WithIncomingQueue(queue string) ReceiverOption {
	return func(o *receiverOptions) {
		o.incomingQueue = queue
	}
}

func WithMaxRetries(n int) ReceiverOption {
	return func(o *receiverOptions) {
		o.maxRetries = int64(n)
	}
}

func WithMinRetryBackoff(d time.Duration) ReceiverOption {
	return func(o *receiverOptions) {
		o.minRetryBackoff = d
	}
}

func WithTopologySetup() ReceiverOption {
	return func(o *receiverOptions) {
		o.setupTopology = true
	}
}

func WithBindingsSetup() ReceiverOption {
	return func(o *receiverOptions) {
		o.setupBindings = true
	}
}

func WithPrefetchCount(c int) ReceiverOption {
	return func(o *receiverOptions) {
		o.prefetchCount = c
	}
}

func WithMarshaler(m rabbitmq.Marshaler) ReceiverOption {
	return func(opts *receiverOptions) {
		opts.marshaler = m
	}
}

func WithLogger(l rabbitmq.Logger) ReceiverOption {
	return func(opts *receiverOptions) {
		opts.logger = l
	}
}

type Receiver struct {
	client       *amqpx.Client
	options      receiverOptions
	consumerName string
}

func NewReceiver(client *amqpx.Client, opts ...ReceiverOption) *Receiver {
	options := defaultReceiverOptions()

	for _, opt := range opts {
		opt(&options)
	}

	return &Receiver{
		client:  client,
		options: options,
	}
}

func (r *Receiver) Setup(ctx context.Context, consumerName string, infos ...eventbus.ServiceInfo) error {
	r.consumerName = consumerName

	if r.options.incomingQueue == "" {
		r.options.incomingQueue = consumerName
	}

	r.options.dlxExchange = r.options.incomingQueue + dlxSuffix
	r.options.waitQueue = r.options.incomingQueue + waitSuffix
	r.options.parkingLotQueue = r.options.incomingQueue + parkingLotSuffix

	r.options.consumerTag = fmt.Sprintf("%s-%s", consumerName, xid.New())

	if !r.options.setupTopology && !r.options.setupBindings {
		return nil
	}

	return r.client.Process(ctx, func(ctx context.Context, conn *connpool.Conn) error {
		if r.options.setupTopology {
			if err := r.setupTopology(conn); err != nil {
				return err
			}
		}

		if r.options.setupBindings {
			if err := r.setupBindings(conn, infos); err != nil {
				return err
			}
		}

		return nil
	})
}

const (
	wait       = "wait"
	retry      = "retry"
	parkingLot = "parkingLot"
)

func (r *Receiver) setupTopology(conn *connpool.Conn) error {
	incomingQueueArgs := amqp.Table{
		"x-dead-letter-exchange":    r.options.dlxExchange,
		"x-dead-letter-routing-key": wait,
	}

	waitQueueArgs := amqp.Table{
		"x-dead-letter-exchange":    r.options.dlxExchange,
		"x-dead-letter-routing-key": retry,
		"x-message-ttl":             r.options.minRetryBackoff.Milliseconds(),
	}

	err := conn.Channel().ExchangeDeclare(r.options.dlxExchange, amqp.ExchangeTopic, true, false, false, false, nil)
	if err != nil {
		return fmt.Errorf("declare exchange %q: %w", r.options.dlxExchange, err)
	}

	_, err = conn.Channel().QueueDeclare(r.options.waitQueue, true, false, false, false, waitQueueArgs)
	if err != nil {
		return fmt.Errorf("declare queue %q: %w", r.options.waitQueue, err)
	}

	_, err = conn.Channel().QueueDeclare(r.options.parkingLotQueue, true, false, false, false, nil)
	if err != nil {
		return fmt.Errorf("declare queue %q: %w", r.options.parkingLotQueue, err)
	}

	_, err = conn.Channel().QueueDeclare(r.options.incomingQueue, true, false, false, false, incomingQueueArgs)
	if err != nil {
		return fmt.Errorf("declare queue %q: %w", r.options.incomingQueue, err)
	}

	if err = conn.Channel().QueueBind(r.options.waitQueue, wait, r.options.dlxExchange, false, nil); err != nil {
		return fmt.Errorf("bind queue %q: %w", r.options.waitQueue, err)
	}

	if err = conn.Channel().QueueBind(r.options.incomingQueue, retry, r.options.dlxExchange, false, nil); err != nil {
		return fmt.Errorf("bind queue %q: %w", r.options.incomingQueue, err)
	}

	if err = conn.Channel().QueueBind(r.options.parkingLotQueue, parkingLot, r.options.dlxExchange, false, nil); err != nil {
		return fmt.Errorf("bind queue %q: %w", r.options.parkingLotQueue, err)
	}

	return nil
}

func (r *Receiver) setupBindings(conn *connpool.Conn, infos []eventbus.ServiceInfo) error {
	for _, info := range infos {
		for _, eventName := range info.Events {
			if err := conn.Channel().QueueBind(r.options.incomingQueue, eventName, info.ServiceName, false, nil); err != nil {
				return fmt.Errorf("bind queue %q to %q: %w", r.options.incomingQueue, info.ServiceName, err)
			}
		}
	}

	return nil
}

func (r *Receiver) Receive(ctx context.Context, processor eventbus.Processor) error {
	return r.client.Process(ctx, func(ctx context.Context, conn *connpool.Conn) error {
		return r.receive(ctx, conn, processor)
	})
}

// The consume errgroup is deliberately detached from ctx: cancellation must
// stop the consumer and let in-flight deliveries drain instead of aborting
// the group, so ctx is watched explicitly in the control goroutine.
//
//nolint:contextcheck
func (r *Receiver) receive(ctx context.Context, conn *connpool.Conn, processor eventbus.Processor) error {
	if err := conn.Channel().Qos(r.options.prefetchCount, 0, false); err != nil {
		return fmt.Errorf("set channel qos: %w", err)
	}

	deliveries, err := conn.Channel().Consume(r.options.incomingQueue, r.options.consumerTag, false, false, false, false, nil)
	if err != nil {
		return fmt.Errorf("consume queue %q: %w", r.options.incomingQueue, err)
	}

	eg, egCtx := errgroup.WithContext(context.Background())

	eg.Go(func() error {
		select {
		case <-ctx.Done():
			if cErr := conn.Channel().Cancel(r.options.consumerTag, false); cErr != nil {
				return fmt.Errorf("cancel consumer %q: %w", r.options.consumerTag, cErr)
			}

			return nil
		case <-egCtx.Done():
			if cErr := conn.Close(); cErr != nil {
				return fmt.Errorf("close connection: %w", cErr)
			}

			return nil
		case connErr := <-conn.NotifyClose(make(chan *amqp.Error)):
			return connErr
		}
	})

	eg.Go(func() error {
		for delivery := range deliveries {
			select {
			case <-egCtx.Done():
				return nil
			default:
				dErr := r.processDelivery(&delivery, processor)

				if ackErr := r.doAcknowledge(conn, &delivery, dErr); ackErr != nil {
					return fmt.Errorf("do acknowledge: %w", ackErr)
				}
			}
		}

		return nil
	})

	return eg.Wait()
}

func (r *Receiver) processDelivery(delivery *amqp.Delivery, processor eventbus.Processor) error {
	md, data, err := r.options.marshaler.Unmarshal(delivery)
	if err == nil {
		return processor(md, data)
	}

	if r.options.logger != nil {
		r.options.logger.Errorf(fmt.Sprintf("unmarshaling event [%+v]: %s", delivery, err))
	}

	return eventbus.NewUnprocessableEventError(err)
}

func (r *Receiver) doAcknowledge(conn *connpool.Conn, d *amqp.Delivery, err error) error {
	switch {
	case err == nil:
		if aErr := d.Ack(false); aErr != nil {
			return fmt.Errorf("ack delivery: %w", aErr)
		}
	case eventbus.IsUnprocessableEventError(err), hasExceededRetryCount(d, r.options.maxRetries):
		return r.putIntoParkingLot(conn, d)
	default:
		if rErr := d.Reject(false); rErr != nil {
			return fmt.Errorf("reject delivery: %w", rErr)
		}
	}

	return nil
}

func (r *Receiver) putIntoParkingLot(conn *connpool.Conn, d *amqp.Delivery) error {
	msg := amqp.Publishing{
		Headers:         d.Headers,
		Type:            d.Type,
		ContentType:     d.ContentType,
		ContentEncoding: d.ContentEncoding,
		DeliveryMode:    d.DeliveryMode,
		Body:            d.Body,
	}

	err := conn.Channel().Publish(r.options.dlxExchange, parkingLot, false, false, msg)
	if err != nil {
		return fmt.Errorf("put into parking lot: %w", err)
	}

	if err = d.Ack(false); err != nil {
		return fmt.Errorf("ack delivery: %w", err)
	}

	return nil
}

func hasExceededRetryCount(d *amqp.Delivery, max int64) bool {
	death, ok := d.Headers["x-death"].([]interface{})
	if !ok {
		return false
	}

	for _, i := range death {
		t, ok := i.(amqp.Table)
		if !ok {
			continue
		}

		if t["queue"] == d.Headers["x-first-death-queue"] {
			count, ok := t["count"].(int64)

			return ok && count >= max
		}
	}

	return false
}
