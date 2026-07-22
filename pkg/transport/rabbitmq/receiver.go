package rabbitmq

import (
	"context"
	"fmt"

	amqp "github.com/rabbitmq/amqp091-go"
	"github.com/rs/xid"
	"golang.org/x/sync/errgroup"

	"github.com/quarks-tech/amqpx"
	"github.com/quarks-tech/amqpx/connpool"

	"github.com/quarks-tech/protoevent-go/pkg/eventbus"
	"github.com/quarks-tech/protoevent-go/pkg/transport/rabbitmq/internal/amqpxlifecycle"
	"github.com/quarks-tech/protoevent-go/pkg/transport/rabbitmq/message"
)

const dlxSuffix = ".dlx"

type receiverOptions struct {
	marshaler      Marshaler
	logger         Logger
	incomingQueue  string
	prefetchCount  int
	consumerTag    string
	setupTopology  bool
	enableDLX      bool
	requeueOnError bool
}

func defaultReceiverOptions() receiverOptions {
	return receiverOptions{
		marshaler:     message.Marshaler{},
		prefetchCount: 3,
	}
}

type ReceiverOption func(o *receiverOptions)

func WithIncomingQueue(queue string) ReceiverOption {
	return func(o *receiverOptions) {
		o.incomingQueue = queue
	}
}

func WithTopologySetup() ReceiverOption {
	return func(o *receiverOptions) {
		o.setupTopology = true
	}
}

func WithRequeue() ReceiverOption {
	return func(o *receiverOptions) {
		o.requeueOnError = true
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

func WithMarshaler(m Marshaler) ReceiverOption {
	return func(opts *receiverOptions) {
		opts.marshaler = m
	}
}

func WithLogger(l Logger) ReceiverOption {
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

	r.options.consumerTag = fmt.Sprintf("%s-%s", consumerName, xid.New())

	if !r.options.setupTopology {
		return nil
	}

	return r.client.Process(ctx, func(ctx context.Context, conn *connpool.Conn) error {
		return r.setupTopology(conn, infos)
	})
}

func (r *Receiver) setupTopology(conn *connpool.Conn, infos []eventbus.ServiceInfo) error {
	var queueDeclareArgs amqp.Table

	if r.options.enableDLX {
		dlxExchange := r.options.incomingQueue + dlxSuffix
		dlxQueue := r.options.incomingQueue + dlxSuffix

		queueDeclareArgs = amqp.Table{
			"x-dead-letter-exchange": dlxExchange,
		}

		err := conn.Channel().ExchangeDeclare(dlxExchange, amqp.ExchangeFanout, true, false, false, false, nil)
		if err != nil {
			return fmt.Errorf("declare exchange %q: %w", dlxExchange, err)
		}

		_, err = conn.Channel().QueueDeclare(dlxQueue, true, false, false, false, nil)
		if err != nil {
			return fmt.Errorf("declare queue %q: %w", dlxQueue, err)
		}

		if err = conn.Channel().QueueBind(dlxQueue, "", dlxExchange, false, nil); err != nil {
			return fmt.Errorf("bind queue %q: %w", dlxQueue, err)
		}
	}

	_, err := conn.Channel().QueueDeclare(r.options.incomingQueue, true, false, false, false, queueDeclareArgs)
	if err != nil {
		return fmt.Errorf("declare queue %q: %w", r.options.incomingQueue, err)
	}

	for _, info := range infos {
		for _, eventName := range info.Events {
			if err = conn.Channel().QueueBind(r.options.incomingQueue, eventName, info.ServiceName, false, nil); err != nil {
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
// stops the consumer while the amqpx command context and borrowed connection
// remain alive until buffered and in-flight deliveries drain.
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

				return doAcknowledge(delivery, dErr, r.options.requeueOnError)
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

func doAcknowledge(m *amqp.Delivery, err error, requeueOnError bool) error {
	switch {
	case err == nil:
		if aErr := m.Ack(false); aErr != nil {
			return fmt.Errorf("ack delivery: %w", aErr)
		}
	case eventbus.IsUnprocessableEventError(err):
		if rErr := m.Reject(false); rErr != nil {
			return fmt.Errorf("reject delivery: %w", rErr)
		}
	default:
		if rErr := m.Reject(requeueOnError); rErr != nil {
			return fmt.Errorf("reject delivery: %w", rErr)
		}
	}

	return nil
}
