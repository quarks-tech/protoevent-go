package rabbitmq

import (
	"context"
	"fmt"

	amqp "github.com/rabbitmq/amqp091-go"
	"github.com/rs/xid"

	"github.com/quarks-tech/amqpx"
	"github.com/quarks-tech/amqpx/connpool"

	"github.com/quarks-tech/protoevent-go/pkg/eventbus"
	"github.com/quarks-tech/protoevent-go/pkg/transport/rabbitmq/internal/consume"
	"github.com/quarks-tech/protoevent-go/pkg/transport/rabbitmq/message"
)

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
		prefetchCount: consume.DefaultPrefetchCount,
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

// WithPrefetchCount sets the channel's QoS prefetch count (default 3).
//
// A non-positive c makes Receive fail. AMQP reads prefetch-count 0 as "no
// specified limit", and this receiver used to pass it straight to Channel.Qos, so
// WithPrefetchCount(0) was a working unlimited consumer. Drain-on-cancel cannot
// honor that — an unbounded prefetch means an unbounded number of buffered
// deliveries to finish inside DrainTimeout — so it is rejected in the caller's own
// terms rather than through amqpx's error, which names a ConsumeSpec the caller
// never touched. The parking-lot receiver rejects it identically.
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
		dlxExchange := r.options.incomingQueue + consume.DLXSuffix
		dlxQueue := r.options.incomingQueue + consume.DLXSuffix

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
	spec := consume.Spec{
		Runtime:     "rabbitmq",
		Queue:       r.options.incomingQueue,
		ConsumerTag: r.options.consumerTag,
		Prefetch:    r.options.prefetchCount,
		Marshaler:   r.options.marshaler,
		Logger:      r.options.logger,
	}

	return consume.Run(shutdownCtx, r.client, spec, processor,
		func(_ context.Context, _ *connpool.Conn, delivery *amqp.Delivery, dErr error) error {
			return doAcknowledge(delivery, dErr, r.options.requeueOnError)
		})
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
