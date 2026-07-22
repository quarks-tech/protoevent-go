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
	"github.com/quarks-tech/protoevent-go/pkg/transport/rabbitmq/internal/amqpxlifecycle"
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

// Receive consumes via amqpx.Client.ProcessWithDrain (drain-on-cancel mode):
// shutdownCtx cancellation stops connection acquisition, while a running
// consume command keeps its connection, drains, and returns nil — so a clean
// shutdown yields nil, not context.Canceled. The client's Config.DrainTimeout
// bounds the drain; size it to the deployment's shutdown budget.
func (r *Receiver) Receive(shutdownCtx context.Context, processor eventbus.Processor) error {
	return r.client.ProcessWithDrain(shutdownCtx, func(commandCtx context.Context, conn *connpool.Conn) error {
		return r.receive(commandCtx, conn, processor)
	})
}

// The consume errgroup is deliberately detached from shutdownCtx: cancellation
// stops the consumer while the borrowed connection remains alive (amqpx drain
// mode) until buffered and in-flight deliveries drain.
func (r *Receiver) receive(
	shutdownCtx context.Context,
	conn *connpool.Conn,
	processor eventbus.Processor,
) error {
	channel := conn.Channel()
	if err := channel.Qos(r.options.prefetchCount, 0, false); err != nil {
		return fmt.Errorf("set channel qos: %w", err)
	}

	// ConsumeWithContext owns a goroutine that may call Channel.Cancel after the
	// command returns. Cancellation is kept inside this joined command instead.
	deliveries, err := channel.Consume(
		r.options.incomingQueue,
		r.options.consumerTag,
		false,
		false,
		false,
		false,
		nil,
	)
	if err != nil {
		return fmt.Errorf("consume queue %q: %w", r.options.incomingQueue, err)
	}

	eg, egCtx := errgroup.WithContext(context.Background())
	workerFailed := make(chan struct{})
	workerDone := make(chan struct{})
	notifyClose := channel.NotifyClose(make(chan *amqp.Error, 1))

	eg.Go(func() error {
		return amqpxlifecycle.WaitForConsumerStop(
			shutdownCtx,
			workerFailed,
			workerDone,
			func() error {
				if cErr := channel.Cancel(r.options.consumerTag, false); cErr != nil {
					return fmt.Errorf("cancel consumer %q: %w", r.options.consumerTag, cErr)
				}

				return nil
			},
			notifyClose,
		)
	})

	eg.Go(func() error {
		defer close(workerDone)

		err := amqpxlifecycle.DrainDeliveries(
			egCtx,
			shutdownCtx,
			deliveries,
			workerFailed,
			func(groupCtx context.Context, delivery *amqp.Delivery) error {
				dErr := r.processDelivery(delivery, processor)
				if groupCtx.Err() != nil {
					return nil
				}

				if ackErr := r.doAcknowledge(shutdownCtx, conn, delivery, dErr); ackErr != nil {
					return fmt.Errorf("do acknowledge: %w", ackErr)
				}

				return nil
			},
		)
		if err != nil {
			return fmt.Errorf("drain deliveries: %w", err)
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

func (r *Receiver) doAcknowledge(ctx context.Context, conn *connpool.Conn, d *amqp.Delivery, err error) error {
	switch {
	case err == nil:
		if aErr := d.Ack(false); aErr != nil {
			return fmt.Errorf("ack delivery: %w", aErr)
		}
	case eventbus.IsUnprocessableEventError(err), hasExceededRetryCount(d, r.options.maxRetries):
		return r.putIntoParkingLot(ctx, conn, d)
	default:
		if rErr := d.Reject(false); rErr != nil {
			return fmt.Errorf("reject delivery: %w", rErr)
		}
	}

	return nil
}

func (r *Receiver) putIntoParkingLot(ctx context.Context, conn *connpool.Conn, d *amqp.Delivery) error {
	msg := amqp.Publishing{
		Headers:         d.Headers,
		Type:            d.Type,
		ContentType:     d.ContentType,
		ContentEncoding: d.ContentEncoding,
		DeliveryMode:    d.DeliveryMode,
		Body:            d.Body,
	}

	// Parking a poison delivery must succeed DURING drain, when ctx is the
	// already-canceled shutdown context (amqpx drain mode hands the command
	// its own canceled ctx) — detach for the broker op, or amqp091's ctx
	// fast-path rejects the publish and the delivery is never parked.
	err := conn.Channel().PublishWithContext(context.WithoutCancel(ctx), r.options.dlxExchange, parkingLot, false, false, msg)
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
