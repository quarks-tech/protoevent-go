package rabbitmq

import (
	"context"
	"fmt"
	"sync"

	amqp "github.com/rabbitmq/amqp091-go"

	"github.com/quarks-tech/amqpx"
	"github.com/quarks-tech/amqpx/connpool"

	"github.com/quarks-tech/protoevent-go/pkg/event"
	"github.com/quarks-tech/protoevent-go/pkg/eventbus"
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

type senderOptions struct {
	deliveryMode uint8
	marshaler    Marshaler
	confirms     bool
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

	// mu guards confirming, the set of pooled channels already switched into
	// publisher-confirm mode. Confirm mode is per channel and survives for the
	// channel's life, so tracking it turns confirm.select into a once-per-channel
	// round trip instead of a once-per-publish one. Send runs concurrently, and
	// the pool hands out one channel per connection.
	mu         sync.Mutex
	confirming map[*amqp.Channel]struct{}
}

func NewSender(client *amqpx.Client, opts ...SenderOption) *Sender {
	options := defaultSenderOptions()

	for _, opt := range opts {
		opt(&options)
	}

	return &Sender{
		client:     client,
		options:    options,
		confirming: make(map[*amqp.Channel]struct{}),
	}
}

// enableConfirms puts ch into publisher-confirm mode, once per channel.
func (s *Sender) enableConfirms(ch *amqp.Channel) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, ok := s.confirming[ch]; ok {
		return nil
	}

	if err := ch.Confirm(false); err != nil {
		return fmt.Errorf("enable publisher confirms: %w", err)
	}

	// Forget channels the pool has retired, so the set tracks the live pool
	// instead of growing with every reconnect for the process lifetime. Swept on
	// the miss path only: that is once per new channel, i.e. exactly as often as
	// the set grows.
	for c := range s.confirming {
		if c.IsClosed() {
			delete(s.confirming, c)
		}
	}

	s.confirming[ch] = struct{}{}

	return nil
}

func (s *Sender) Setup(ctx context.Context, desc *eventbus.ServiceDesc) error {
	return s.client.Process(ctx, func(ctx context.Context, conn *connpool.Conn) error {
		if err := conn.Channel().ExchangeDeclare(desc.ServiceName, amqp.ExchangeTopic, true, false, false, false, nil); err != nil {
			return fmt.Errorf("declare exchange %q: %w", desc.ServiceName, err)
		}

		return nil
	})
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

	return s.client.Process(ctx, func(ctx context.Context, conn *connpool.Conn) error {
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
		// mandatory stays false, deliberately: in pub/sub an exchange with no
		// matching binding is a topic nobody subscribes to, not a failure. What is
		// being caught here is the broker refusing the publish, not the absence of
		// consumers.
		if err := s.enableConfirms(ch); err != nil {
			return err
		}

		conf, err := ch.PublishWithDeferredConfirmWithContext(ctx, exchange, routingKey, false, false, mess)
		if err != nil {
			return fmt.Errorf("publish to exchange %q: %w", exchange, err)
		}

		acked, err := conf.WaitContext(ctx)
		if err != nil {
			return fmt.Errorf("await publisher confirm from exchange %q: %w", exchange, err)
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
	})
}
