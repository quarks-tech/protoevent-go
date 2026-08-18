package rabbitmq

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
)

// recordingLogger is a Logger that is not DefaultLogger, so the tests below can
// tell "the option was applied" from "the default survived".
type recordingLogger struct{ msgs []string }

func (l *recordingLogger) Errorf(format string, _ ...any) { l.msgs = append(l.msgs, format) }

// TestDefaultReceiverLoggerIsNotNil pins the one thing the default has to be.
// It used to be nil, and the only place the receiver logs through it is
// consume.Spec.Logger, which nil-guards — so an event that would not unmarshal
// was rejected (dropped, or dead-lettered under WithDLX) with no error to the
// caller and no log line anywhere to find it by.
func TestDefaultReceiverLoggerIsNotNil(t *testing.T) {
	if defaultReceiverOptions().logger == nil {
		t.Fatal("default logger is nil: an undecodable delivery would be rejected silently")
	}
}

func TestWithLoggerNilIsNoop(t *testing.T) {
	o := defaultReceiverOptions()
	l := &recordingLogger{}
	WithLogger(l)(&o)
	WithLogger(nil)(&o)

	if o.logger != Logger(l) {
		t.Fatal("WithLogger(nil) replaced a previously set logger")
	}
}

// TestWithLoggerNilKeepsTheDefault covers the other order: WithLogger(nil) on a
// fresh option set must not strip the default back to silence.
func TestWithLoggerNilKeepsTheDefault(t *testing.T) {
	o := defaultReceiverOptions()
	WithLogger(nil)(&o)

	if o.logger == nil {
		t.Fatal("WithLogger(nil) cleared the default logger")
	}
}

// TestSetupRejectsDLXWithoutTopologySetup pins that an option which cannot take
// effect fails loudly instead of reporting success.
//
// WithDLX() alone was silently inert: Setup returns early when setupTopology is
// false, BEFORE the block that declares the dead-letter exchange and sets
// x-dead-letter-exchange on the queue. So a caller who added WithDLX precisely
// because rejected deliveries were disappearing got no .dlx exchange, no queue
// argument, and no error — and the option they reached for was their evidence the
// problem was fixed.
//
// It cannot be honored silently either: x-dead-letter-exchange is an argument of
// queue.declare, so only whoever declares the queue can set it.
//
// A nil client is safe here: the guard must fire before any broker call, which is
// itself part of the contract.
func TestSetupRejectsDLXWithoutTopologySetup(t *testing.T) {
	r := NewReceiver(nil, WithDLX())

	err := r.Setup(context.Background(), "consumer")
	if err == nil {
		t.Fatal("Setup with WithDLX() and no WithTopologySetup() = nil; the option declares no " +
			"dead-letter exchange and changes nothing, so reporting success tells the caller " +
			"their rejected deliveries are now safe when they are still being discarded")
	}
	if !strings.Contains(err.Error(), "WithTopologySetup") {
		t.Fatalf("Setup error = %v, want it to name WithTopologySetup as the remedy", err)
	}

	// The remedy must actually be accepted (it reaches the client, so only the
	// validation is exercised here — a nil client would panic past this point).
	r2 := NewReceiver(nil, WithDLX(), WithTopologySetup())
	if r2.options.enableDLX != true || r2.options.setupTopology != true {
		t.Fatal("WithDLX()+WithTopologySetup() did not set both flags")
	}
}

// TestRequeueDelayGrowsWithDeliveryCount pins the pacing that keeps a finite retry
// budget from being spent at broker speed.
//
// A quorum queue's x-delivery-limit is consumed by REDELIVERIES, so an unpaced requeue
// loop exhausted a 20-delivery budget in 13.9ms and the broker discarded the message.
// Growing the delay with x-delivery-count turns that budget into a window measured in
// minutes, which is what lets an ordinary downstream fault clear before the event is
// destroyed.
func TestRequeueDelayGrowsWithDeliveryCount(t *testing.T) {
	const (
		base = 200 * time.Millisecond
		max  = 15 * time.Second
	)

	for _, tc := range []struct {
		name  string
		count any
		want  time.Duration
	}{
		{"first delivery, header absent", nil, base},
		{"first redelivery", int64(1), 400 * time.Millisecond},
		{"third redelivery", int64(3), 1600 * time.Millisecond},
		{"capped", int64(20), max},
		// The header's integer width varies with the broker and with anything between
		// it and this consumer, the same reason the parking lot normalizes x-death.
		{"int32 header", int32(2), 800 * time.Millisecond},
		{"int header", 2, 800 * time.Millisecond},
		{"uint64 header", uint64(2), 800 * time.Millisecond},
		// A nonsense value must not produce a nonsense delay.
		{"unparseable header", "many", base},
		{"negative header", int64(-5), base},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := &amqp.Delivery{}
			if tc.count != nil {
				d.Headers = amqp.Table{deliveryCountHeader: tc.count}
			}

			if got := requeueDelay(d, base, max); got != tc.want {
				t.Fatalf("requeueDelay = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestRequeueDelayIsDisabledByAZeroBase pins the opt-out: WithRequeueBackoff(0, ...)
// restores the immediate requeue, for a deployment that has bounded the loop some other
// way (a parking lot, or a queue with no delivery limit).
func TestRequeueDelayIsDisabledByAZeroBase(t *testing.T) {
	d := &amqp.Delivery{Headers: amqp.Table{deliveryCountHeader: int64(5)}}

	if got := requeueDelay(d, 0, time.Minute); got != 0 {
		t.Fatalf("requeueDelay with a zero base = %v, want 0", got)
	}
}

// TestRequeueBackoffIsSkippedDuringDrain pins that the pacing never delays a shutdown.
//
// doAcknowledge receives the SHUTDOWN context (see consume.Ack), so a cancellation means
// the process is draining: the delivery has to go back to the broker at once rather than
// being held for a delay nobody is waiting for. Holding it would add the backoff to
// every shutdown and push the relay past its termination grace period.
func TestRequeueBackoffIsSkippedDuringDrain(t *testing.T) {
	opts := defaultReceiverOptions()
	opts.requeueBackoffBase = 30 * time.Second
	opts.requeueBackoffMax = 30 * time.Second

	rec := &recordingAcknowledger{}
	d := &amqp.Delivery{Acknowledger: rec, Headers: amqp.Table{deliveryCountHeader: int64(3)}}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	start := time.Now()
	if err := doAcknowledge(ctx, d, errors.New("transient"), opts); err != nil {
		t.Fatalf("doAcknowledge: %v", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("doAcknowledge held the delivery for %v during drain, want an immediate requeue", elapsed)
	}
	if !rec.rejected || !rec.requeue {
		t.Fatalf("delivery was not requeued (rejected=%v requeue=%v)", rec.rejected, rec.requeue)
	}
}

// recordingAcknowledger captures the disposition applied to a delivery.
type recordingAcknowledger struct {
	acked    bool
	rejected bool
	requeue  bool
}

func (r *recordingAcknowledger) Ack(uint64, bool) error {
	r.acked = true

	return nil
}

func (r *recordingAcknowledger) Nack(uint64, bool, bool) error { return nil }

func (r *recordingAcknowledger) Reject(_ uint64, requeue bool) error {
	r.rejected = true
	r.requeue = requeue

	return nil
}

// TestRequeueBudgetSpansARealisticOutage pins the SIZE of the retry window, which is the
// property the growth exists for and the one an integration test with a short fault
// cannot discriminate.
//
// A quorum queue's default x-delivery-limit is 20, and the delays between those
// deliveries are the whole window a fault has to clear in. Flat pacing at the base delay
// would give 20 * 200ms = 4s — enough to pass a test with a 1.5s blip, and nowhere near
// enough for the faults that actually happen: a database failover, a rolling restart of a
// dependency, a leader election. Doubling to the cap spans minutes instead.
func TestRequeueBudgetSpansARealisticOutage(t *testing.T) {
	const (
		base          = defaultRequeueBackoffBase
		max           = defaultRequeueBackoffMax
		deliveryLimit = 20
		// A dependency failover or rolling restart is tens of seconds. The budget must
		// comfortably exceed that, or the event is destroyed while the fault is still
		// being resolved.
		wantAtLeast = 60 * time.Second
	)

	var total time.Duration
	for n := range deliveryLimit {
		d := &amqp.Delivery{Headers: amqp.Table{deliveryCountHeader: int64(n)}}
		total += requeueDelay(d, base, max)
	}

	if total < wantAtLeast {
		t.Fatalf("the %d-delivery retry budget spans only %v, want at least %v: a fault lasting "+
			"longer than that exhausts the budget and the broker discards the event",
			deliveryLimit, total, wantAtLeast)
	}
	t.Logf("a %d-delivery budget spans %v of retry window", deliveryLimit, total)
}
