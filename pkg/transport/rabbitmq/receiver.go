package rabbitmq

import (
	"context"
	"errors"
	"fmt"
	"time"

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

	// requeueBackoffBase and requeueBackoffMax bound how fast a transient failure may
	// be retried. See WithRequeueBackoff.
	requeueBackoffBase time.Duration
	requeueBackoffMax  time.Duration
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
		// Requeueing is paced by WithRequeueBackoff rather than being immediate. An
		// unthrottled loop is not merely wasteful on a quorum queue: those carry an
		// x-delivery-limit (RabbitMQ 4.x applies one by default), and retrying at broker
		// speed spends the whole budget in milliseconds — measured at 21 attempts in
		// 13.9ms — after which the broker DISCARDS the message. A downstream blip
		// shorter than a network round trip destroyed the event, which is the exact case
		// this default exists to survive. See WithoutRequeue to opt out entirely.
		requeueOnError:     true,
		requeueBackoffBase: defaultRequeueBackoffBase,
		requeueBackoffMax:  defaultRequeueBackoffMax,
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

// WithPrefetchCount sets the channel's QoS prefetch count (default 16).
//
// It does NOT set handler concurrency: deliveries are processed one at a time on a
// single goroutine whatever the prefetch is. What it buys is how many deliveries are
// already buffered when that goroutine blocks — which is why it is the second knob on
// the requeue-pacing stall (see WithRequeueBackoff), where the consumer clears
// prefetch-1 messages per pacing delay.
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
		func(ctx context.Context, _ *connpool.Conn, delivery *amqp.Delivery, dErr error) error {
			return doAcknowledge(ctx, delivery, dErr, r.options)
		})
}

// Default pacing for requeued transient failures. The base doubles per redelivery up
// to the cap, so a budget of 20 deliveries spans minutes instead of milliseconds: a
// blip clears on an early attempt, while a genuinely stuck message still reaches its
// limit (and a dead-letter exchange, if one is attached) in bounded time.
//
// The cap is 5s, not the 15s it started at, because the cap is also what the rest of
// the queue pays. The delay is served on the single consume goroutine (see
// WithRequeueBackoff), so during an episode the consumer clears prefetch-1 messages
// per delay, and at the cap that ratio is the consumer's whole throughput. 5s keeps
// the 20-delivery budget at ~76s — still four orders of magnitude above the 13.9ms
// this pacing was introduced to fix, and longer than the blips it exists to survive —
// while tripling what gets through behind a stuck message.
const (
	defaultRequeueBackoffBase = 200 * time.Millisecond
	defaultRequeueBackoffMax  = 5 * time.Second
)

// deliveryCountHeader is set by quorum queues on each redelivery, and is what lets the
// backoff grow with the number of failed attempts rather than being flat.
const deliveryCountHeader = "x-delivery-count"

// WithRequeueBackoff paces requeued transient failures: the delay before a delivery is
// returned to the broker starts at base and doubles per redelivery, capped at max.
//
// It exists because the retry budget is finite and the broker sets the pace. A quorum
// queue's x-delivery-limit is consumed by REDELIVERIES, so an unpaced requeue loop
// exhausts it at broker speed and the message is discarded — measured at 21 attempts in
// 13.9ms, so any downstream fault lasting longer than a few milliseconds was fatal to
// the event. Pacing turns that budget into a window long enough for the fault to clear.
//
// THE DELAY STOPS THE WHOLE CONSUMER, not just the delivery being held. It is served
// by a sleep on the single goroutine that reads deliveries — amqpx hands one delivery
// to the handler and does not read the next until it returns — so prefetch buys
// buffering, never concurrency. While a delivery is being paced, nothing else on the
// queue is processed.
//
// What that costs, exactly: during an episode the consumer clears prefetch-1 other
// messages per delay, because those are the ones already buffered when the sleep
// starts. Measured against a real quorum queue with one permanently-failing message
// among healthy traffic, at the old 15s cap and the old prefetch of 3, the healthy
// messages arrived in PAIRS on the backoff ladder — 200ms, 600ms, 1.4s, 3.0s, 6.2s,
// 12.6s, 25.4s, 40.4s, 55.4s, 70.4s — an aggregate 0.13/s against a 4264/s baseline,
// for as long as the failing message's delivery budget lasted. With pacing disabled
// the identical scenario cleared in 11ms, so the stall is this sleep and nothing else.
//
// So the two knobs that bound the damage are this cap and the prefetch, and the
// defaults are set together for that reason (see consume.DefaultPrefetchCount). The
// pacing itself is cheap for what it was built for: a 2s dependency outage across 100
// messages cleared 2.7s after the first delivery.
//
// If a persistently-failing message must not stall the queue AT ALL, this receiver is
// the wrong shape — use parkinglot.Receiver, where the wait is served by a broker-side
// queue TTL rather than by the consume goroutine.
//
// A zero or negative base disables pacing and restores the immediate-requeue behavior.
func WithRequeueBackoff(base, max time.Duration) ReceiverOption {
	return func(o *receiverOptions) {
		o.requeueBackoffBase = base
		o.requeueBackoffMax = max
	}
}

// deliveryCount reports how many times this delivery has already been delivered, from
// the quorum-queue header. Absent (classic queues, or the first delivery) is 0, which
// yields the base delay — still enough to stop a tight loop from burning CPU.
//
// The header arrives as one of several integer widths depending on the broker and on
// anything between it and this consumer, so every AMQP integer type is accepted; the
// same normalization the parking lot applies to x-death.
func deliveryCount(m *amqp.Delivery) int {
	// Clamped: the delay is capped long before this many redeliveries, so a huge or
	// hostile value only needs to be turned into "plenty" rather than converted
	// faithfully — which also keeps the widening conversions below in range.
	const clamp = 64

	var n int64
	switch v := m.Headers[deliveryCountHeader].(type) {
	case int:
		n = int64(v)
	case int8:
		n = int64(v)
	case int16:
		n = int64(v)
	case int32:
		n = int64(v)
	case int64:
		n = v
	case uint8:
		n = int64(v)
	case uint16:
		n = int64(v)
	case uint32:
		n = int64(v)
	case uint64:
		if v > clamp {
			return clamp
		}
		n = int64(v)
	default:
		return 0
	}

	if n < 0 {
		return 0
	}
	if n > clamp {
		return clamp
	}

	return int(n)
}

// requeueDelay is the pause before returning a transiently-failed delivery, doubling
// with the redelivery count and capped at max.
func requeueDelay(m *amqp.Delivery, base, max time.Duration) time.Duration {
	if base <= 0 {
		return 0
	}

	n := deliveryCount(m)
	if n < 0 {
		n = 0
	}
	// Shift, but never past the cap: 1<<n overflows well before a realistic
	// delivery limit is reached.
	delay := base
	for range n {
		if delay >= max {
			return max
		}
		delay *= 2
	}
	if max > 0 && delay > max {
		return max
	}

	return delay
}

func doAcknowledge(ctx context.Context, m *amqp.Delivery, err error, o receiverOptions) error {
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
		// Pace the requeue. ctx is the SHUTDOWN context (see consume.Ack), so a
		// cancellation here means the process is draining: return the delivery at once
		// rather than holding it for a delay nobody is waiting for.
		if o.requeueOnError {
			if d := requeueDelay(m, o.requeueBackoffBase, o.requeueBackoffMax); d > 0 {
				timer := time.NewTimer(d)
				defer timer.Stop()
				select {
				case <-timer.C:
				case <-ctx.Done():
				}
			}
		}
		if rErr := m.Reject(o.requeueOnError); rErr != nil {
			return fmt.Errorf("reject delivery: %w", rErr)
		}
	}

	return nil
}
