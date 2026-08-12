// Package consume holds the per-delivery machinery the two RabbitMQ receivers
// share: the drain-aware consume loop, the requeue every path that leaves a
// delivery unacked depends on, the prefetch guard, and the AMQP header
// normalization the binary marshaler needs as well.
//
// It exists because the receivers used to carry byte-identical copies of all of
// it, differing in exactly one thing — the acknowledge policy — which is why that
// is the one piece Run takes as a parameter. Two copies of a drain-time
// requeue/ack sequence is two places for a shutdown bug to be fixed in only one.
//
// Being internal also lets the prefetch default stop being part of the public API:
// it was exported purely so the parking-lot receiver, in a sibling package, could
// reach it.
package consume

import (
	"context"
	"fmt"

	amqp "github.com/rabbitmq/amqp091-go"

	"github.com/quarks-tech/amqpx"
	"github.com/quarks-tech/amqpx/connpool"

	"github.com/quarks-tech/protoevent-go/pkg/event"
	"github.com/quarks-tech/protoevent-go/pkg/eventbus"
)

// DLXSuffix names the dead-letter exchange (and queue) a receiver derives from its
// incoming queue. Both receivers derive it, and they must agree: a plain receiver
// and a parking-lot receiver pointed at the same queue name would otherwise declare
// two different broker-side topologies for the same logical DLX.
const DLXSuffix = ".dlx"

// DefaultPrefetchCount is the prefetch a receiver starts with when the caller
// configures none.
const DefaultPrefetchCount = 3

// Marshaler is the delivery-decoding half of rabbitmq.Marshaler. Only Unmarshal is
// named here because the consume loop never publishes; a rabbitmq.Marshaler
// satisfies it structurally.
type Marshaler interface {
	Unmarshal(d *amqp.Delivery) (*event.Metadata, []byte, error)
}

// Logger mirrors rabbitmq.Logger, which a receiver's configured logger satisfies
// structurally. Declared here rather than imported to keep this package free of a
// dependency on its own parent.
type Logger interface {
	Errorf(format string, args ...any)
}

// Ack is a receiver's acknowledge policy: the one thing the two consume loops
// actually do differently. err is the delivery's processing result (nil on
// success), and the policy owns the delivery's disposition — ack, reject, or park
// it — returning an error only when that disposition could not be applied.
//
// ctx is the SHUTDOWN context, not the consume group's: a policy that has to talk
// to the broker (parking a poison delivery) must still work during the drain, when
// the shutdown context is already canceled, and detaches for the broker op itself.
type Ack func(ctx context.Context, conn *connpool.Conn, d *amqp.Delivery, err error) error

// Spec is what Run needs to open and service one subscription.
type Spec struct {
	// Runtime names the calling receiver in errors the caller sees:
	// "rabbitmq" or "parkinglot".
	Runtime     string
	Queue       string
	ConsumerTag string
	// Prefetch is the configured prefetch; Run rejects a non-positive one.
	Prefetch  int
	Marshaler Marshaler
	Logger    Logger
}

// Run consumes spec.Queue via amqpx.Client.ConsumeWithDrain, decoding each
// delivery and handing the result to ack.
//
// amqpx owns the whole consumer lifecycle — QoS, Consume, the stop-reason
// multiplexer (shutdown / handler failure / broker close), the drain, and
// requeueing the prefetched deliveries the drain never hands over. What is left
// here is the per-delivery half of the contract: a delivery amqpx HAS handed over
// belongs to this handler, so every path out of it must leave that delivery either
// acknowledged or requeued.
func Run(
	shutdownCtx context.Context,
	client *amqpx.Client,
	spec Spec,
	processor eventbus.Processor,
	ack Ack,
) error {
	if err := checkPrefetch(spec.Runtime, spec.Prefetch); err != nil {
		return err
	}

	consumeSpec := amqpx.ConsumeSpec{
		Queue:       spec.Queue,
		ConsumerTag: spec.ConsumerTag,
		Prefetch:    spec.Prefetch,
	}

	return client.ConsumeWithDrain(shutdownCtx, consumeSpec,
		func(groupCtx context.Context, conn *connpool.Conn, delivery *amqp.Delivery) error {
			dErr := processDelivery(groupCtx, spec, delivery, processor)
			if groupCtx.Err() != nil {
				// The group is stopping, so this delivery's disposition cannot be
				// trusted to land; requeue it rather than leaving it unacked on a
				// channel that returns to the pool. amqpx only requeues deliveries it
				// never handed to us — this one it did.
				Requeue(delivery)

				return nil
			}

			// shutdownCtx, not groupCtx: an acknowledge policy that publishes (the
			// parking lot) must still succeed during drain, when shutdownCtx is
			// already canceled — the policy detaches for the broker op itself.
			if ackErr := ack(shutdownCtx, conn, delivery, dErr); ackErr != nil {
				// Our own disposition failed, so the delivery is still unacked and
				// amqpx will not touch it (by contract, a delivery handed to the
				// handler belongs to the handler).
				Requeue(delivery)

				return ackErr
			}

			return nil
		})
}

// processDelivery decodes a delivery and runs the subscriber's processor over it.
// An undecodable delivery is reported as an unprocessable event, which is what
// makes an acknowledge policy dead-letter (or park) it instead of retrying it
// forever.
func processDelivery(
	ctx context.Context,
	spec Spec,
	delivery *amqp.Delivery,
	processor eventbus.Processor,
) error {
	md, data, err := spec.Marshaler.Unmarshal(delivery)
	if err == nil {
		return processor(ctx, md, data)
	}

	if spec.Logger != nil {
		spec.Logger.Errorf("unmarshaling event [%+v]: %s", delivery, err)
	}

	return eventbus.NewUnprocessableEventError(err)
}

// Requeue hands a delivery back to the broker for immediate redelivery.
//
// Best-effort: the only reason Nack fails here is a channel that is already gone,
// and a closed channel requeues every unacked delivery on it broker-side anyway.
func Requeue(delivery *amqp.Delivery) {
	_ = delivery.Nack(false, true)
}

// checkPrefetch rejects a non-positive prefetch, in the caller's own terms.
//
// AMQP reads prefetch-count 0 as "no specified limit", and the receivers used to
// pass it straight to Channel.Qos, so WithPrefetchCount(0) was a working unlimited
// consumer. Drain-on-cancel cannot honor that: an unbounded prefetch means an
// unbounded number of buffered deliveries to finish inside DrainTimeout, so amqpx
// requires a positive bound — and its own error names ConsumeSpec, a type the
// caller never touched.
//
// It is an ERROR rather than a silent fallback to the default: a consumer that
// quietly runs with a prefetch nobody asked for is a throughput mystery later,
// whereas a subscription that fails at startup names the option to fix.
func checkPrefetch(runtime string, configured int) error {
	if configured > 0 {
		return nil
	}

	return fmt.Errorf("%s: WithPrefetchCount must be > 0, got %d "+
		"(unlimited prefetch is unsupported under drain-on-cancel: the shutdown drain must be bounded)",
		runtime, configured)
}

// HeaderString reads an AMQP header that carries text, in either shape the wire
// produces.
//
// An AMQP longstr field decodes as string OR []byte depending on the broker and on
// anything between it and this consumer — a shovel, a federation link, a proxy.
// Two call sites depend on this, and both were bugs before it existed:
//
//   - the binary marshaler's core attributes. The bare `.(string)` assertion this
//     replaced reported such a header as MISSING, so the delivery was wrapped as an
//     UnprocessableEventError and every message from that publisher was silently
//     dead-lettered (or parked) with its attributes sitting right there, readable.
//   - the parking lot's x-death bookkeeping, where comparing two []byte values
//     through an interface panics with "comparing uncomparable type []uint8" —
//     crashing the consumer on the retry path that is meant to be the safe one.
//
// A value with no string identity at all reports ok=false; a caller that wants a
// plain string can ignore ok, but must then not treat the resulting "" as a match
// for another absent field.
func HeaderString(v any) (string, bool) {
	switch v := v.(type) {
	case string:
		return v, true
	case []byte:
		return string(v), true
	default:
		return "", false
	}
}
