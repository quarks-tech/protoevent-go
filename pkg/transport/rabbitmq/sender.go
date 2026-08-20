package rabbitmq

import (
	"context"
	"errors"
	"fmt"

	amqp "github.com/rabbitmq/amqp091-go"

	"github.com/quarks-tech/amqpx"
	"github.com/quarks-tech/amqpx/connpool"

	"github.com/quarks-tech/protoevent-go/pkg/event"
	"github.com/quarks-tech/protoevent-go/pkg/eventbus"
	"github.com/quarks-tech/protoevent-go/pkg/transport/rabbitmq/internal/publish"
	"github.com/quarks-tech/protoevent-go/pkg/transport/rabbitmq/message"
)

const (
	DeliveryModeTransient  = 1
	DeliveryModePersistent = 2
)

func WithTransientDeliveryMode() SenderOption {
	return func(opts *senderOptions) {
		opts.deliveryMode = DeliveryModeTransient
	}
}

func WithMessageMarshaler(m Marshaler) SenderOption {
	return func(opts *senderOptions) {
		opts.marshaler = m
	}
}

// WithoutPublisherConfirms turns off publisher confirms, making Send return as
// soon as the frame is written instead of waiting for the broker to acknowledge
// it.
//
// Only for a caller that can tolerate silent message loss. Without confirms the
// broker's rejection of a publish — a missing exchange, a resource alarm, a
// failed persist — arrives asynchronously on a channel this Send call has already
// returned success from, so an outbox relay commits its offset (or resume token)
// past an event the broker discarded, and the at-least-once guarantee the outbox
// exists to provide is void with nothing anywhere reporting it.
func WithoutPublisherConfirms() SenderOption {
	return func(opts *senderOptions) {
		opts.confirms = false
	}
}

// WithMandatoryPublish makes Send fail when the exchange routed the message to NO
// queue, instead of reporting success.
//
// Publisher confirms do NOT cover this. RabbitMQ acks a publish once the exchange
// has determined the routing set, INCLUDING when that set is empty, so with
// confirms alone Send returns nil for a message no queue ever received. For an
// outbox relay that answer is what authorizes committing the offset (or persisting
// the resume token) past the event, so the event is gone and OnDrained counts it as
// sent. basic.return, which requires the mandatory flag, is the only signal.
//
// Off by default, because in pub/sub an exchange with no matching binding is
// normally a topic nobody subscribes to yet, not a failure — and with this option a
// publish to such a topic becomes an error, which for a relay means the lane stops
// and retries that event until a binding exists. That is the right trade when every
// event has a known consumer and losing one is unacceptable (the outbox case), and
// the wrong one for genuinely optional fan-out.
//
// Turn it on if any of these can happen to you: an event type published before its
// consumer's binding exists, a binding dropped during a topology migration, or a
// routing-key typo. Each of those loses events silently by default.
//
// Requires publisher confirms (the default): detecting the return needs the ack to
// order against. With WithoutPublisherConfirms it is rejected at construction, since
// there is nothing to correlate against and the combination would silently do
// nothing.
func WithMandatoryPublish() SenderOption {
	return func(opts *senderOptions) {
		opts.mandatory = true
	}
}

type senderOptions struct {
	deliveryMode uint8
	marshaler    Marshaler
	confirms     bool
	mandatory    bool
}

func defaultSenderOptions() senderOptions {
	return senderOptions{
		marshaler:    message.Marshaler{},
		deliveryMode: DeliveryModePersistent,
		confirms:     true,
	}
}

type SenderOption func(opts *senderOptions)

type commandProcessor interface {
	Process(context.Context, amqpx.Command) error
}

type Sender struct {
	client  commandProcessor
	options senderOptions

	// confirms is the per-channel publisher-confirm bookkeeping, shared with the
	// parking-lot receiver via internal/publish. It used to be a copy of that
	// machinery in each file, and the copies diverged.
	confirms *publish.Confirms
}

func NewSender(client *amqpx.Client, opts ...SenderOption) *Sender {
	options := defaultSenderOptions()

	for _, opt := range opts {
		opt(&options)
	}

	return &Sender{
		client:   client,
		options:  options,
		confirms: publish.NewConfirms(),
	}
}

// ErrUnroutable reports a publish the exchange routed to no queue at all. Returned
// only under WithMandatoryPublish; by default such a publish is acked by the broker
// and reported as success. See that option for why it is off by default.
var ErrUnroutable = errors.New("rabbitmq: publish was not routed to any queue")

// Setup declares the service's exchange.
//
// It is also where an incoherent option pair is reported. WithMandatoryPublish needs
// the confirm ack to order the basic.return against (see returnWatch), so with
// WithoutPublisherConfirms it could detect nothing — and an option that silently
// detects nothing is worse than no option, because it reads as protection.
func (s *Sender) Setup(ctx context.Context, desc *eventbus.ServiceDesc) error {
	if s.options.mandatory && !s.options.confirms {
		return errors.New("rabbitmq: WithMandatoryPublish requires publisher confirms: an " +
			"unroutable publish is detected by correlating basic.return with the publish's " +
			"confirm, so with WithoutPublisherConfirms nothing would be detected; drop one of " +
			"the two options")
	}

	return poolExhaustionHint(s.client.Process(ctx, func(ctx context.Context, conn *connpool.Conn) error {
		if err := conn.Channel().ExchangeDeclare(desc.ServiceName, amqp.ExchangeTopic, true, false, false, false, nil); err != nil {
			return fmt.Errorf("declare exchange %q: %w", desc.ServiceName, err)
		}

		return nil
	}))
}

func (s *Sender) Send(ctx context.Context, meta *event.Metadata, data []byte) error {
	mess, err := s.options.marshaler.Marshal(meta, data)
	if err != nil {
		return fmt.Errorf("marshal to rabbitmq message: %w", err)
	}

	mess.DeliveryMode = s.options.deliveryMode

	// The service part of the event type is the exchange, the event part the routing
	// key. event.SplitType is the one definition of the event-type shape, shared with
	// the publisher's write-side guard and the subscriber's routing, so the three
	// cannot drift.
	//
	// Routed off meta.Type, the authoritative event type — a marshaler is free to
	// leave amqp.Publishing.Type empty or rewrite it. meta.Type may come from a
	// persisted outbox row, so a malformed value must error, not panic.
	exchange, routingKey, err := event.SplitType(meta.Type)
	if err != nil {
		return err
	}

	return poolExhaustionHint(s.client.Process(ctx, func(ctx context.Context, conn *connpool.Conn) error {
		// Nothing below is worth a broker round trip on a dead context, and the
		// confirm-mode handshake would take one before the publish ever refused.
		if err := ctx.Err(); err != nil {
			return err
		}

		ch := conn.Channel()

		if !s.options.confirms {
			if err := ch.PublishWithContext(ctx, exchange, routingKey, false, false, mess); err != nil {
				return fmt.Errorf("publish to exchange %q: %w", exchange, err)
			}

			return nil
		}

		// Wait for the broker's confirm before reporting success. An unconfirmed
		// publish returns nil the moment the frame is written, so a broker-side
		// rejection — the 404 channel exception for an exchange that does not exist
		// yet, a resource alarm, a failed persist — arrives asynchronously, after
		// this Send already told its caller the event was delivered. For an outbox
		// relay that answer is what authorizes committing the offset (or persisting
		// the resume token) past the event, so the event is dropped and swept while
		// Observer.OnDrained counts it as sent.
		//
		// mandatory is off unless WithMandatoryPublish: in pub/sub an exchange with no
		// matching binding is normally a topic nobody subscribes to, not a failure.
		// But confirms do NOT cover that case — RabbitMQ acks a publish whose routing
		// set is EMPTY — so by default an event no queue received is reported as
		// delivered, and a relay commits its position past it. See
		// WithMandatoryPublish for when that trade is the wrong one.
		if err := s.confirms.Enable(ctx, ch); err != nil {
			return err
		}

		// Under mandatory publishing, hold this channel for the publish/confirm pair:
		// a basic.return names no publish, so attributing one needs a single publish
		// in flight on the channel (see returnWatch).
		var watch *publish.Watch
		if s.options.mandatory {
			watch = s.confirms.Returns(ch)
			defer watch.Lock()()
		}

		conf, err := ch.PublishWithDeferredConfirmWithContext(ctx, exchange, routingKey, s.options.mandatory, false, mess)
		if err != nil {
			return fmt.Errorf("publish to exchange %q: %w", exchange, err)
		}

		acked, err := conf.WaitContext(ctx)
		if err != nil {
			return fmt.Errorf("await publisher confirm from exchange %q: %w", exchange, err)
		}
		// Checked after the ack, which is the ordering that makes it attributable:
		// the broker sends basic.return before the ack for the same publish.
		if watch != nil {
			if ret, ok := watch.Took(); ok {
				return fmt.Errorf("%w: exchange %q, routing key %q (%d %s)",
					ErrUnroutable, exchange, routingKey, ret.ReplyCode, ret.ReplyText)
			}
		}
		if !acked {
			// A nack, or a channel exception that closed the channel with the
			// publish still outstanding (amqp091 nacks everything unconfirmed when
			// the channel goes down). Either way the broker does not have the
			// message, and saying so is what keeps the relay from advancing past it.
			return fmt.Errorf("publish to exchange %q was not confirmed by the broker "+
				"(nacked, or the channel closed before the confirm arrived)", exchange)
		}

		return nil
	}))
}

// poolExhaustionHint annotates connection-pool exhaustion with the cause the raw
// error cannot name.
//
// amqpx reports it as "connection pool timeout", which reads like broker trouble. The
// overwhelmingly common cause is neither the broker nor load: a running subscription
// holds one pool connection EXCLUSIVELY for its entire life (Receive ->
// ConsumeWithDrain -> ProcessWithDrain -> withConn), and PoolSize defaults to
// runtime.GOMAXPROCS(0), which on Go 1.25+ is cgroup-aware. So a 1-CPU pod gets a pool
// of one, and a service that subscribes and publishes through a single client starves
// every one of its own publishes, permanently — the exact shape this library's own
// examples show. Two subscriptions need PoolSize >= 3 before a publish can succeed.
//
// It cannot be caught at construction: amqpx.Client exposes no way to read PoolSize
// (its exported surface is Process, ProcessWithDrain and Close). Naming the cause in
// the error is the only place this library can intervene, so it does.
//
// The sentinel is preserved for errors.Is, and any other error passes through
// untouched.
func poolExhaustionHint(err error) error {
	if err == nil || !errors.Is(err, connpool.ErrPoolTimeout) {
		return err
	}

	return fmt.Errorf("%w: every connection in the amqpx pool is busy. A running subscription holds "+
		"one pool connection for as long as it runs, and PoolSize defaults to GOMAXPROCS "+
		"(cgroup-aware, so a 1-CPU pod gets 1) — a service that subscribes and publishes through "+
		"ONE client therefore starves its own publishes. Set PoolSize >= subscriptions+1, or give "+
		"the publisher its own amqpx.Client", err)
}

var _ eventbus.BatchSender = (*Sender)(nil)

// SendBatch publishes a run of events on ONE channel and waits for their confirms
// together, instead of paying a full publish-and-confirm round trip per event.
//
// Why it exists: a publish costs almost nothing and the confirm costs everything.
// Measured against a RabbitMQ 4 quorum queue, this Sender ran at 86,139 events/s
// with confirms off and 1,155/s with them on — the broker round trip is 99% of a
// send. An outbox relay drains serially from one goroutine, so that per-event round
// trip was the whole pipeline's ceiling: ~918 events/s sustained on loopback, and
// on the same curve 129/s at the 5ms confirm an ordinary cross-AZ quorum queue
// answers in. Overlapping the waits replaces "one round trip per event" with "one
// round trip per batch".
//
// It does NOT weaken what a confirm means. Every message is still individually
// confirmed by the broker; only the WAITING is overlapped. The count returned is the
// contiguous acked prefix, so a caller advancing a durable position past it advances
// past nothing the broker did not acknowledge — see eventbus.BatchSender for why the
// prefix must never be optimistic.
//
// Two configurations fall back to sending serially rather than refusing:
//
//   - WithoutPublisherConfirms, where there is nothing to overlap.
//   - WithMandatoryPublish, which needs exactly one publish in flight per channel to
//     attribute a basic.return (see returnWatch). Pipelining and unroutable-publish
//     detection are mutually exclusive on one channel, and correctness wins.
func (s *Sender) SendBatch(ctx context.Context, msgs []eventbus.Outgoing) (int, error) {
	if len(msgs) == 0 {
		return 0, nil
	}
	if !s.options.confirms || s.options.mandatory {
		return s.sendSerial(ctx, msgs)
	}

	// Marshal and route BEFORE opening the channel, so a message this Sender can
	// never encode is reported without having published anything after it. A
	// failure at index i still lets [0,i) go: those are good messages and the
	// caller may legitimately advance past them.
	prepared := make([]preparedPublish, 0, len(msgs))
	prepErr := error(nil)
	for _, m := range msgs {
		p, err := s.prepare(m)
		if err != nil {
			prepErr = err

			break
		}
		prepared = append(prepared, p)
	}
	if len(prepared) == 0 {
		return 0, prepErr
	}

	sent, err := s.publishPipelined(ctx, prepared)
	if err != nil {
		return sent, err
	}
	// Every prepared message was confirmed. If preparation stopped early, that
	// message is the failure the caller must act on.
	return sent, prepErr
}

// sendSerial is the fallback path: Send, one message at a time, reporting the
// contiguous prefix that succeeded.
func (s *Sender) sendSerial(ctx context.Context, msgs []eventbus.Outgoing) (int, error) {
	for i, m := range msgs {
		if err := s.Send(ctx, m.Metadata, m.Data); err != nil {
			return i, err
		}
	}

	return len(msgs), nil
}

// preparedPublish is one message already marshaled and routed, so the publish loop
// does no work that could fail for a reason unrelated to the broker.
type preparedPublish struct {
	exchange   string
	routingKey string
	publishing amqp.Publishing
}

func (s *Sender) prepare(m eventbus.Outgoing) (preparedPublish, error) {
	mess, err := s.options.marshaler.Marshal(m.Metadata, m.Data)
	if err != nil {
		return preparedPublish{}, fmt.Errorf("marshal to rabbitmq message: %w", err)
	}
	mess.DeliveryMode = s.options.deliveryMode

	exchange, routingKey, err := event.SplitType(m.Metadata.Type)
	if err != nil {
		return preparedPublish{}, err
	}

	return preparedPublish{exchange: exchange, routingKey: routingKey, publishing: mess}, nil
}

// publishPipelined writes every publish on one channel, then collects the confirms
// in publish order, returning the length of the contiguous acked prefix.
func (s *Sender) publishPipelined(ctx context.Context, prepared []preparedPublish) (int, error) {
	var (
		acked   int
		ackErr  error
		confirm = make([]*amqp.DeferredConfirmation, 0, len(prepared))
	)

	procErr := s.client.Process(ctx, func(ctx context.Context, conn *connpool.Conn) error {
		// Reset per attempt: amqpx may retry this callback on a fresh connection,
		// and a prefix counted against a channel that no longer exists is not a
		// prefix the broker acknowledged.
		acked, ackErr, confirm = 0, nil, confirm[:0]

		if err := ctx.Err(); err != nil {
			return err
		}

		ch := conn.Channel()
		if err := s.confirms.Enable(ctx, ch); err != nil {
			return err
		}

		// Publish everything first. A publish that fails takes the channel with it,
		// so stop there and go collect what was already accepted: those confirms
		// still resolve (amqp091 nacks anything outstanding when the channel goes
		// down), and the prefix that acked is real.
		var pubErr error
		for i := range prepared {
			p := &prepared[i]
			c, err := ch.PublishWithDeferredConfirmWithContext(ctx, p.exchange, p.routingKey, false, false, p.publishing)
			if err != nil {
				pubErr = fmt.Errorf("publish to exchange %q: %w", p.exchange, err)

				break
			}
			confirm = append(confirm, c)
		}

		// Collect in publish order, and STOP at the first message the broker did
		// not ack. Continuing past it would count a later ack toward the prefix,
		// which is exactly the gap-as-progress the contract forbids.
		for i, c := range confirm {
			ok, err := c.WaitContext(ctx)
			if err != nil {
				ackErr = fmt.Errorf("await publisher confirm from exchange %q: %w", prepared[i].exchange, err)

				break
			}
			if !ok {
				// A nack, or a channel exception that closed the channel with the
				// publish still outstanding. Either way the broker does not have it.
				ackErr = fmt.Errorf("publish to exchange %q was not confirmed by the broker "+
					"(nacked, or the channel closed before the confirm arrived)", prepared[i].exchange)

				break
			}
			acked = i + 1
		}

		if ackErr != nil {
			return ackErr
		}

		return pubErr
	})

	return acked, poolExhaustionHint(procErr)
}
