package rabbitmq

import (
	"context"
	"errors"
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
		logger:        DefaultLogger(),
		// Requeue on a transient handler error, rather than reject-without-requeue.
		//
		// The old default DESTROYED the event: Reject(requeue=false) on a queue with
		// no dead-letter exchange makes the broker discard the message, so a handler
		// that returned an error because a database timed out or a downstream
		// answered 503 — the most ordinary failure a consumer has — silently lost the
		// event, with a log line as the only trace. An outbox upstream can guarantee
		// the event reaches the broker; it cannot help once the consumer throws it
		// away.
		//
		// A permanently-unprocessable event is a different case and is still rejected
		// without requeue (see doAcknowledge): retrying it can never succeed, so
		// requeueing it would spin forever. That distinction is the caller's to make,
		// by returning eventbus.NewUnprocessableEventError.
		//
		// The cost of this default is that a handler failing on a message the broker
		// keeps redelivering retries in a tight loop. Bound it with WithDLX (a
		// dead-letter exchange plus x-message-ttl gives delayed retry) or the
		// parkinglot receiver, and see WithoutRequeue to opt back out.
		requeueOnError: true,
	}
}

type ReceiverOption func(o *receiverOptions)

// WithIncomingQueue names the queue this receiver consumes. Defaults to the
// subscriber's consumer name, which is what Setup receives.
func WithIncomingQueue(queue string) ReceiverOption {
	return func(o *receiverOptions) {
		o.incomingQueue = queue
	}
}

// WithTopologySetup makes Setup declare this consumer's topology: the queue, its
// bindings to each subscribed service exchange, and — with WithDLX — the
// dead-letter exchange and queue.
//
// Without it Setup declares NOTHING and only records the queue/consumer names, for
// deployments whose topology is managed externally (Terraform, an operator, a
// migration job). Note that the queue must then already carry
// x-dead-letter-exchange itself: that argument can only be set at queue-declare
// time, so WithDLX cannot add it to a queue this receiver did not declare — which is
// why combining WithDLX with externally-managed topology is rejected rather than
// silently ignored.
func WithTopologySetup() ReceiverOption {
	return func(o *receiverOptions) {
		o.setupTopology = true
	}
}

// WithRequeue requeues a delivery whose handler returned a transient error, so the
// broker redelivers it instead of discarding it.
//
// Deprecated: this is the default; the option is a no-op kept so existing callers keep
// compiling. See WithoutRequeue for the opposite.
func WithRequeue() ReceiverOption {
	return func(o *receiverOptions) {
		o.requeueOnError = true
	}
}

// WithoutRequeue rejects a delivery whose handler returned a transient error
// WITHOUT requeueing it, restoring the pre-default behavior.
//
// Only for a queue that has a dead-letter exchange: without one the broker DISCARDS
// the rejected message, so every transient handler failure — a database timeout, a
// downstream 503 — silently loses the event. Pair it with WithDLX, or with a queue
// declared elsewhere that carries x-dead-letter-exchange.
func WithoutRequeue() ReceiverOption {
	return func(o *receiverOptions) {
		o.requeueOnError = false
	}
}

// WithDLX declares a dead-letter exchange and queue for this consumer, and sets
// x-dead-letter-exchange on the incoming queue, so a delivery rejected without
// requeue lands somewhere an operator can inspect instead of being discarded.
//
// Requires WithTopologySetup: x-dead-letter-exchange is a queue-declare argument
// and cannot be applied to a queue this receiver does not declare. Setup rejects the
// combination rather than accepting an option that would do nothing.
//
// The dead-letter exchange is a fanout named "<queue>.dlx", fronting one queue of
// the same name. The parking-lot receiver derives the SAME name as a TOPIC
// exchange, so the two must not share an incoming queue name — see
// consume.DLXSuffix and the 406 reported by consume.DLXConflictError.
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

// WithLogger routes the receiver's error reports somewhere other than
// DefaultLogger. A nil l is ignored: it used to mean "log nowhere", which is now
// what the receiver must never silently be — pass a discarding Logger to opt
// into silence deliberately.
func WithLogger(l Logger) ReceiverOption {
	return func(opts *receiverOptions) {
		if l != nil {
			opts.logger = l
		}
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
		// WithDLX without WithTopologySetup used to be silently inert: this early
		// return happens BEFORE the DLX block in setupTopology, so no .dlx exchange
		// was declared, x-dead-letter-exchange was never set, and Setup reported
		// success. A caller who added WithDLX precisely to stop losing rejected
		// deliveries kept losing them, and the option they reached for was the
		// evidence they had fixed it.
		//
		// It cannot be honored here either: x-dead-letter-exchange is an argument of
		// queue.declare, so it can only be set by whoever declares the queue. An
		// error is the only honest answer.
		if r.options.enableDLX {
			return errors.New("rabbitmq: WithDLX requires WithTopologySetup: x-dead-letter-exchange " +
				"can only be set when this receiver declares the queue itself, so WithDLX alone would " +
				"declare no dead-letter exchange and silently change nothing; either add " +
				"WithTopologySetup(), or set x-dead-letter-exchange on the externally-managed queue " +
				"and drop WithDLX()")
		}

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

		// Fanout, fronting one dead-letter queue. The parking-lot receiver declares
		// the SAME derived name as a topic exchange, so the two must not share an
		// incoming queue name — see consume.DLXSuffix, and DLXConflictError for the
		// 406 that says so.
		err := conn.Channel().ExchangeDeclare(dlxExchange, amqp.ExchangeFanout, true, false, false, false, nil)
		if err != nil {
			return consume.DLXConflictError(dlxExchange, err)
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
//
// POOL SIZING. This call occupies one amqpx pool connection EXCLUSIVELY for as long
// as it runs, because a consumer is a long-lived command rather than a round trip. So
// a client's PoolSize must exceed the number of subscriptions running on it, or
// nothing else can borrow a connection: publishes fail with connpool.ErrPoolTimeout
// (annotated — see poolExhaustionHint in sender.go), permanently, not transiently.
//
// The default makes this easy to hit: PoolSize defaults to runtime.GOMAXPROCS(0),
// which on Go 1.25+ is cgroup-aware, so a 1-CPU pod gets a pool of ONE and a service
// that both subscribes and publishes through one client starves its own publishes.
// Size PoolSize to subscriptions + 1, or give the publisher its own amqpx.Client.
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
