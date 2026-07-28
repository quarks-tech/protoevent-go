package rabbitmq

import (
	"context"
	"fmt"

	amqp "github.com/rabbitmq/amqp091-go"
	"github.com/rs/xid"

	"github.com/quarks-tech/amqpx"
	"github.com/quarks-tech/amqpx/connpool"

	"github.com/quarks-tech/protoevent-go/pkg/eventbus"
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

// Receive consumes via amqpx.Client.ConsumeWithDrain (drain-on-cancel mode):
// shutdownCtx cancellation cancels the consumer and drains in-flight and
// prefetched deliveries before returning, so a clean shutdown yields nil, not
// context.Canceled. The client's Config.DrainTimeout bounds the drain; size it to
// the deployment's shutdown budget.
//
// amqpx owns the whole consumer lifecycle — QoS, Consume, the stop-reason
// multiplexer (shutdown / handler failure / broker close), the drain, and
// requeueing the prefetched deliveries the drain never hands over. This receiver
// supplies only the per-delivery policy, which is the one thing it and the
// parking-lot receiver do differently.
func (r *Receiver) Receive(shutdownCtx context.Context, processor eventbus.Processor) error {
	if r.options.prefetchCount <= 0 {
		// Caught here, in the caller's own terms. AMQP reads prefetch-count 0 as
		// "no specified limit", and this receiver used to pass it straight to
		// Channel.Qos, so WithPrefetchCount(0) was a working unlimited consumer.
		// Drain mode cannot honor that: an unbounded prefetch means an unbounded
		// number of buffered deliveries to finish inside DrainTimeout, so amqpx
		// requires a positive bound — and its own error names ConsumeSpec, a type
		// the caller never touched.
		return fmt.Errorf("rabbitmq: WithPrefetchCount must be > 0, got %d "+
			"(unlimited prefetch is unsupported under drain-on-cancel: the shutdown drain must be bounded)",
			r.options.prefetchCount)
	}

	spec := amqpx.ConsumeSpec{
		Queue:       r.options.incomingQueue,
		ConsumerTag: r.options.consumerTag,
		Prefetch:    r.options.prefetchCount,
	}

	return r.client.ConsumeWithDrain(shutdownCtx, spec,
		func(groupCtx context.Context, _ *connpool.Conn, delivery *amqp.Delivery) error {
			dErr := r.processDelivery(delivery, processor)
			if groupCtx.Err() != nil {
				// The group is stopping, so this delivery's disposition cannot be
				// trusted to land; requeue it rather than leaving it unacked on a
				// channel that returns to the pool. amqpx only requeues deliveries it
				// never handed to us — this one it did.
				requeue(delivery)

				return nil
			}

			if err := doAcknowledge(delivery, dErr, r.options.requeueOnError); err != nil {
				// Our own disposition failed, so the delivery is still unacked and
				// amqpx will not touch it (by contract, a delivery handed to the
				// handler belongs to the handler).
				requeue(delivery)

				return err
			}

			return nil
		})
}

// requeue hands a delivery back to the broker for immediate redelivery.
//
// Best-effort: the only reason Nack fails here is a channel that is already gone,
// and a closed channel requeues every unacked delivery on it broker-side anyway.
func requeue(delivery *amqp.Delivery) {
	_ = delivery.Nack(false, true)
}

func (r *Receiver) processDelivery(delivery *amqp.Delivery, processor eventbus.Processor) error {
	md, data, err := r.options.marshaler.Unmarshal(delivery)
	if err == nil {
		return processor(md, data)
	}

	if r.options.logger != nil {
		r.options.logger.Errorf("unmarshaling event [%+v]: %s", delivery, err)
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
