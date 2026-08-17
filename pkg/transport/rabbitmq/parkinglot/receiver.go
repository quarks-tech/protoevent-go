package parkinglot

import (
	"context"
	"fmt"
	"math"
	"strconv"
	"sync"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
	"github.com/rs/xid"

	"github.com/quarks-tech/amqpx"
	"github.com/quarks-tech/amqpx/connpool"

	"github.com/quarks-tech/protoevent-go/pkg/eventbus"
	"github.com/quarks-tech/protoevent-go/pkg/transport/rabbitmq"
	"github.com/quarks-tech/protoevent-go/pkg/transport/rabbitmq/internal/consume"
	"github.com/quarks-tech/protoevent-go/pkg/transport/rabbitmq/message"
)

const (
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
		prefetchCount:   consume.DefaultPrefetchCount,
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

	// mu guards confirming, the set of pooled channels already switched into
	// publisher-confirm mode. Confirm mode is per channel and lasts for the
	// channel's life, so tracking it makes confirm.select a once-per-channel round
	// trip instead of a once-per-park one. Deliveries are handled concurrently, and
	// the pool hands out one channel per connection. Mirrors rabbitmq.Sender.
	mu         sync.Mutex
	confirming map[*amqp.Channel]struct{}
}

func NewReceiver(client *amqpx.Client, opts ...ReceiverOption) *Receiver {
	options := defaultReceiverOptions()

	for _, opt := range opts {
		opt(&options)
	}

	return &Receiver{
		client:     client,
		options:    options,
		confirming: make(map[*amqp.Channel]struct{}),
	}
}

// enableConfirms puts ch into publisher-confirm mode, once per channel.
func (r *Receiver) enableConfirms(ch *amqp.Channel) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if _, ok := r.confirming[ch]; ok {
		return nil
	}

	if err := ch.Confirm(false); err != nil {
		return fmt.Errorf("enable publisher confirms: %w", err)
	}

	if r.confirming == nil {
		// A Receiver built as a struct literal rather than through NewReceiver
		// (tests do this) has no map; assigning into a nil one panics.
		r.confirming = make(map[*amqp.Channel]struct{})
	}

	// Forget channels the pool has retired, so the set tracks the live pool
	// instead of growing with every reconnect for the process lifetime. Swept on
	// the miss path only: that is once per new channel, i.e. exactly as often as
	// the set grows.
	for c := range r.confirming {
		if c.IsClosed() {
			delete(r.confirming, c)
		}
	}

	r.confirming[ch] = struct{}{}

	return nil
}

func (r *Receiver) Setup(ctx context.Context, consumerName string, infos ...eventbus.ServiceInfo) error {
	r.consumerName = consumerName

	if r.options.incomingQueue == "" {
		r.options.incomingQueue = consumerName
	}

	r.options.dlxExchange = r.options.incomingQueue + consume.DLXSuffix
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

	// Topic, routing wait / retry / parkingLot. The plain rabbitmq.Receiver
	// declares the SAME derived name as a fanout exchange, so the two must not
	// share an incoming queue name — see consume.DLXSuffix, and DLXConflictError
	// for the 406 that says so.
	err := conn.Channel().ExchangeDeclare(r.options.dlxExchange, amqp.ExchangeTopic, true, false, false, false, nil)
	if err != nil {
		return consume.DLXConflictError(r.options.dlxExchange, err)
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

// Receive consumes via amqpx.Client.ConsumeWithDrain (drain-on-cancel mode):
// shutdownCtx cancellation cancels the consumer and drains in-flight and
// prefetched deliveries before returning, so a clean shutdown yields nil, not
// context.Canceled. The client's Config.DrainTimeout bounds the drain; size it to
// the deployment's shutdown budget.
//
// amqpx owns the whole consumer lifecycle — QoS, Consume, the stop-reason
// multiplexer, the drain, and requeueing the prefetched deliveries the drain never
// hands over. This receiver supplies only the per-delivery policy: a failed
// delivery goes to the parking lot instead of being rejected.
func (r *Receiver) Receive(shutdownCtx context.Context, processor eventbus.Processor) error {
	// A non-positive prefetch fails the subscription — see
	// rabbitmq.WithPrefetchCount for why.
	spec := consume.Spec{
		Runtime:     "parkinglot",
		Queue:       r.options.incomingQueue,
		ConsumerTag: r.options.consumerTag,
		Prefetch:    r.options.prefetchCount,
		Marshaler:   r.options.marshaler,
		Logger:      r.options.logger,
	}

	// ctx here is the SHUTDOWN context: parking must still succeed during drain,
	// when it is already canceled — putIntoParkingLot detaches for the broker op
	// itself.
	return consume.Run(shutdownCtx, r.client, spec, processor,
		func(ctx context.Context, conn *connpool.Conn, delivery *amqp.Delivery, dErr error) error {
			if ackErr := r.doAcknowledge(ctx, conn, delivery, dErr); ackErr != nil {
				return fmt.Errorf("do acknowledge: %w", ackErr)
			}

			return nil
		})
}

func (r *Receiver) doAcknowledge(ctx context.Context, conn *connpool.Conn, d *amqp.Delivery, err error) error {
	switch {
	case err == nil:
		if aErr := d.Ack(false); aErr != nil {
			return fmt.Errorf("ack delivery: %w", aErr)
		}
	case eventbus.IsUnprocessableEventError(err), r.hasExceededRetryCount(d):
		return r.putIntoParkingLot(ctx, conn, d)
	default:
		if rErr := d.Reject(false); rErr != nil {
			return fmt.Errorf("reject delivery: %w", rErr)
		}
	}

	return nil
}

// parkConfirmTimeout bounds the wait for the broker's confirm of a park.
//
// The publish runs on a DETACHED context (see below), so without a bound of its
// own a broker that never answers would block this delivery's handler forever,
// and with it the drain and the whole shutdown. It is deliberately generous: the
// cost of waiting is one stalled delivery, while giving up early requeues a
// message the broker may yet have accepted, so it is parked twice.
const parkConfirmTimeout = 30 * time.Second

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
	// fast-path rejects the publish and the delivery is never parked. The
	// detached context gets its own deadline so the wait below cannot be
	// unbounded.
	pubCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), parkConfirmTimeout)
	defer cancel()

	// Wait for the broker's confirm before acking the original. This publish is
	// the one place in the whole receiver where the message exists ONLY on the
	// wire: the Ack below destroys the sole remaining copy of a delivery whose
	// entire purpose is to be kept for a human to look at. Unconfirmed, the
	// publish returns nil the instant the frame is written, so a broker that
	// refuses it — a resource alarm, a DLX that was never declared (404), a failed
	// persist — reports that asynchronously on a channel nobody reads, long after
	// the original was acked and gone. rabbitmq.Sender takes exactly this
	// precaution for ordinary publishes; losing a poison message is worse, not
	// better.
	//
	// mandatory stays false. Detecting an unroutable park would need the return to
	// be correlated to THIS publish, and the channel comes from a shared pool that
	// handles deliveries concurrently, so a NotifyReturn listener cannot tell whose
	// return it saw. A missing binding is a topology error that WithTopologySetup
	// prevents; what confirms catch is the broker refusing the publish outright.
	if err := r.enableConfirms(conn.Channel()); err != nil {
		return fmt.Errorf("put into parking lot: %w", err)
	}

	conf, err := conn.Channel().PublishWithDeferredConfirmWithContext(
		pubCtx, r.options.dlxExchange, parkingLot, false, false, msg)
	if err != nil {
		return fmt.Errorf("put into parking lot: %w", err)
	}

	acked, err := conf.WaitContext(pubCtx)
	if err != nil {
		return fmt.Errorf("await parking-lot publisher confirm: %w", err)
	}
	if !acked {
		// A nack, or a channel exception that closed the channel with the publish
		// still outstanding (amqp091 nacks everything unconfirmed when the channel
		// goes down). Either way the broker does not have the message, so the
		// original must NOT be acked — it is requeued by the consume loop and
		// retried instead.
		return fmt.Errorf("parking-lot publish to exchange %q was not confirmed by the broker "+
			"(nacked, or the channel closed before the confirm arrived)", r.options.dlxExchange)
	}

	if err = d.Ack(false); err != nil {
		// The park IS durable at this point, so the failed ack must not be reported
		// as a failed park: the consume loop requeues on any error from this policy,
		// and the redelivered message would be parked a second time, filling the
		// human-inspection queue with duplicates of one event on every lap. Say so
		// instead, and report success — the broker redelivers the unacked original
		// on its own once the channel closes, and the parked copy is what the
		// operator needs either way.
		if r.options.logger != nil {
			r.options.logger.Errorf("parkinglot: parked delivery %q but could not ack the original "+
				"(it will be redelivered and re-parked once): %s", d.MessageId, err)
		}
	}

	return nil
}

// hasExceededRetryCount reports whether the delivery has used up its retry
// budget and should be parked instead of dead-lettered for another lap.
//
// It is consulted for TRANSIENT handler failures only (an unprocessable event is
// parked by the branch before it), so its bias matters: parking on a count it
// cannot read would cut the retry budget to one lap, dead-lettering healthy
// traffic to the parking lot during any downstream blip and requiring manual
// reprocessing. An unreadable count therefore keeps retrying — but says so, so
// that a broker re-encoding the header is diagnosable instead of silently
// capping retries at infinity.
func (r *Receiver) hasExceededRetryCount(d *amqp.Delivery) bool {
	death, ok := d.Headers["x-death"].([]any)
	if !ok {
		return false
	}

	// An absent/unreadable first-death queue matches no entry: without this, the
	// empty-string normalization below would make a malformed entry that also lacks
	// a queue compare equal to it.
	firstDeathQueue, _ := consume.HeaderString(d.Headers["x-first-death-queue"])
	if firstDeathQueue == "" {
		return false
	}

	for _, i := range death {
		t, ok := i.(amqp.Table)
		if !ok {
			continue
		}

		// Compared as normalized strings, never with interface ==: AMQP longstr
		// fields decode as string OR []byte depending on the broker and any proxy
		// in between, and comparing two []byte values through an interface panics
		// with "comparing uncomparable type []uint8" — crashing the consumer on
		// the retry path that is meant to be the safe one.
		if queue, _ := consume.HeaderString(t["queue"]); queue == firstDeathQueue {
			count, ok := deathCount(t["count"])
			if !ok {
				if r.options.logger != nil {
					r.options.logger.Errorf("parkinglot: unreadable x-death count %#v (%T) on delivery %q: "+
						"retry budget cannot be evaluated, continuing to retry", t["count"], t["count"], d.MessageId)
				}

				return false
			}

			return count >= r.options.maxRetries
		}
	}

	return false
}

// deathCount normalizes the x-death `count` field. RabbitMQ encodes it as AMQP
// `long` (int64), but proxies, shovels and federation links have been observed
// re-encoding it — as a narrower or unsigned integer, as a float after a JSON
// round-trip, or as a decimal string.
func deathCount(v any) (int64, bool) {
	switch c := v.(type) {
	case int64:
		return c, true
	case int32:
		return int64(c), true
	case int16:
		return int64(c), true
	case int8:
		return int64(c), true
	case int:
		return int64(c), true
	case uint64:
		if c > math.MaxInt64 {
			return math.MaxInt64, true
		}

		return int64(c), true
	case uint32:
		return int64(c), true
	case uint16:
		return int64(c), true
	case uint8:
		return int64(c), true
	case float64:
		// A JSON round-trip turns the count into a float. Only an exact integral
		// value is a count; anything else is not something to guess at.
		if c != math.Trunc(c) || c < math.MinInt64 || c >= math.MaxInt64 {
			return 0, false
		}

		return int64(c), true
	case float32:
		return deathCount(float64(c))
	case string:
		n, err := strconv.ParseInt(c, 10, 64)
		if err != nil {
			return 0, false
		}

		return n, true
	case []byte:
		return deathCount(string(c))
	default:
		return 0, false
	}
}
