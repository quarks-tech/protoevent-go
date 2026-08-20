package rabbitmq

import (
	"context"
	"errors"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"

	"github.com/quarks-tech/protoevent-go/pkg/eventbus"
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

// TestDoAcknowledgeWaitsTheComputedDelay pins the half of the pacing that only the
// ACKNOWLEDGE path can prove: that the delay computed by requeueDelay is actually
// served before the delivery goes back to the broker.
//
// requeueDelay is covered above as a pure function, but a function returning the right
// duration is not the same claim as doAcknowledge waiting it — deleting the timer and
// keeping the arithmetic passes every test above. The only other coverage is the
// broker-backed head-of-line test, which needs Docker, takes seconds, and measures the
// aggregate effect rather than one delivery's delay.
//
// Inside a synctest bubble the wait is exact rather than approximate: the bubble's
// clock advances only when every goroutine in it is blocked, so the elapsed time IS the
// timer's duration with no scheduler jitter, no sleep-longer-than-asked, and no
// wall-clock cost. That is what lets this assert equality on a 15-second delay in a test
// that finishes instantly.
func TestDoAcknowledgeWaitsTheComputedDelay(t *testing.T) {
	for _, tc := range []struct {
		name  string
		count int64
		want  time.Duration
	}{
		{"first delivery", 0, 200 * time.Millisecond},
		{"third redelivery", 3, 1600 * time.Millisecond},
		{"capped", 20, 15 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				opts := defaultReceiverOptions()
				opts.requeueBackoffBase = 200 * time.Millisecond
				opts.requeueBackoffMax = 15 * time.Second

				rec := &recordingAcknowledger{}
				d := &amqp.Delivery{
					Acknowledger: rec,
					Headers:      amqp.Table{deliveryCountHeader: tc.count},
				}

				start := time.Now()
				if err := doAcknowledge(t.Context(), d, errors.New("transient"), opts); err != nil {
					t.Fatalf("doAcknowledge: %v", err)
				}
				if elapsed := time.Since(start); elapsed != tc.want {
					t.Fatalf("doAcknowledge held the delivery for %v, want exactly %v — the "+
						"requeue must be paced by requeueDelay before it is returned to the broker",
						elapsed, tc.want)
				}
				if !rec.rejected || !rec.requeue {
					t.Fatalf("delivery was not requeued after the delay (rejected=%v requeue=%v)",
						rec.rejected, rec.requeue)
				}
			})
		})
	}
}

// TestDoAcknowledgeDoesNotWaitOnDispositionsThatAreNotRequeues pins that the pacing is
// scoped to the one case it exists for.
//
// A successful handler and a permanently-unprocessable event both dispose of the
// delivery immediately: neither is going to be redelivered, so neither has a
// delivery-limit budget to protect, and delaying either would add the backoff to the
// steady-state path. Asserting a zero advance inside the bubble is what makes this
// airtight — against a real clock, "fast" and "not waiting at all" are the same
// measurement.
func TestDoAcknowledgeDoesNotWaitOnDispositionsThatAreNotRequeues(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want func(*recordingAcknowledger) bool
		desc string
	}{
		{
			name: "handler succeeded",
			err:  nil,
			want: func(r *recordingAcknowledger) bool { return r.acked },
			desc: "acked",
		},
		{
			name: "unprocessable event",
			err:  eventbus.NewUnprocessableEventError(errors.New("bad payload")),
			want: func(r *recordingAcknowledger) bool { return r.rejected && !r.requeue },
			desc: "rejected without requeue",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				opts := defaultReceiverOptions()
				opts.requeueBackoffBase = time.Minute
				opts.requeueBackoffMax = time.Minute

				rec := &recordingAcknowledger{}
				d := &amqp.Delivery{
					Acknowledger: rec,
					Headers:      amqp.Table{deliveryCountHeader: int64(5)},
				}

				start := time.Now()
				if err := doAcknowledge(t.Context(), d, tc.err, opts); err != nil {
					t.Fatalf("doAcknowledge: %v", err)
				}
				if elapsed := time.Since(start); elapsed != 0 {
					t.Fatalf("doAcknowledge waited %v before it %s; only a REQUEUE is paced, "+
						"because only a redelivery spends the queue's delivery-limit budget",
						elapsed, tc.desc)
				}
				if !tc.want(rec) {
					t.Fatalf("delivery was not %s (acked=%v rejected=%v requeue=%v)",
						tc.desc, rec.acked, rec.rejected, rec.requeue)
				}
			})
		})
	}
}

// TestDoAcknowledgeSkipsTheDelayWhenPacingIsDisabled pins the opt-out end to end:
// WithRequeueBackoff(0, ...) must remove the wait itself, not just make requeueDelay
// return zero.
func TestDoAcknowledgeSkipsTheDelayWhenPacingIsDisabled(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		opts := defaultReceiverOptions()
		opts.requeueBackoffBase = 0
		opts.requeueBackoffMax = 0

		rec := &recordingAcknowledger{}
		d := &amqp.Delivery{Acknowledger: rec, Headers: amqp.Table{deliveryCountHeader: int64(7)}}

		start := time.Now()
		if err := doAcknowledge(t.Context(), d, errors.New("transient"), opts); err != nil {
			t.Fatalf("doAcknowledge: %v", err)
		}
		if elapsed := time.Since(start); elapsed != 0 {
			t.Fatalf("doAcknowledge waited %v with pacing disabled, want an immediate requeue", elapsed)
		}
		if !rec.rejected || !rec.requeue {
			t.Fatalf("delivery was not requeued (rejected=%v requeue=%v)", rec.rejected, rec.requeue)
		}
	})
}

// TestRequeueBackoffIsSkippedDuringDrain pins that the pacing never delays a shutdown.
//
// doAcknowledge receives the SHUTDOWN context (see consume.Ack), so a cancellation means
// the process is draining: the delivery has to go back to the broker at once rather than
// being held for a delay nobody is waiting for. Holding it would add the backoff to
// every shutdown and push the relay past its termination grace period.
//
// Asserted inside a synctest bubble as an EXACT zero advance. Against the real clock
// this could only check that a 30s delay finished in under a second, which passes just
// as happily if the drain shortens the wait instead of skipping it — and a shortened
// wait, multiplied by the prefetched deliveries a drain works through, is still a
// shutdown overrun.
func TestRequeueBackoffIsSkippedDuringDrain(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		opts := defaultReceiverOptions()
		opts.requeueBackoffBase = 30 * time.Second
		opts.requeueBackoffMax = 30 * time.Second

		rec := &recordingAcknowledger{}
		d := &amqp.Delivery{Acknowledger: rec, Headers: amqp.Table{deliveryCountHeader: int64(3)}}

		ctx, cancel := context.WithCancel(t.Context())
		cancel()

		start := time.Now()
		if err := doAcknowledge(ctx, d, errors.New("transient"), opts); err != nil {
			t.Fatalf("doAcknowledge: %v", err)
		}
		if elapsed := time.Since(start); elapsed != 0 {
			t.Fatalf("doAcknowledge held the delivery for %v during drain, want an immediate requeue", elapsed)
		}
		if !rec.rejected || !rec.requeue {
			t.Fatalf("delivery was not requeued (rejected=%v requeue=%v)", rec.rejected, rec.requeue)
		}
	})
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

// TestDefaultsBoundThePacingStall pins the two defaults against the property they
// were chosen for, so neither can be retuned in isolation.
//
// The requeue pacing sleep blocks the single consume goroutine, so while one
// message keeps failing the consumer clears prefetch-1 OTHER messages per delay,
// and at the cap that ratio is its entire throughput. The pair shipped before this
// (prefetch 3, cap 15s) worked out to 0.13 msg/s against a 4264 msg/s baseline.
//
// The bound below is deliberately loose — it is a floor on a known-bad regime, not
// a performance target. What it prevents is raising the cap or lowering the
// prefetch without noticing that the other one has to move too.
func TestDefaultsBoundThePacingStall(t *testing.T) {
	const minRate = 2.0 // messages per second behind a permanently-failing one

	o := defaultReceiverOptions()
	rate := float64(o.prefetchCount-1) / o.requeueBackoffMax.Seconds()

	if rate < minRate {
		t.Errorf("prefetch %d with a %v backoff cap lets only %.2f msg/s through behind a "+
			"permanently-failing message, want >= %.1f.\n"+
			"The pacing sleep stops the whole consume goroutine, so the consumer clears "+
			"prefetch-1 messages per delay. Raising the cap or lowering the prefetch requires "+
			"moving the other one to compensate — or moving persistent-failure workloads to "+
			"parkinglot.Receiver, where the wait is served broker-side.",
			o.prefetchCount, o.requeueBackoffMax, rate, minRate)
	}

	// The other half of the trade: the cap must still buy a retry budget long enough
	// to outlast an ordinary blip. A quorum queue's default x-delivery-limit is 20,
	// and the pacing exists because that budget used to burn in 13.9ms.
	const deliveryLimit = 20
	var budget time.Duration
	d := o.requeueBackoffBase
	for range deliveryLimit {
		budget += min(d, o.requeueBackoffMax)
		d *= 2
	}
	if budget < 30*time.Second {
		t.Errorf("the %d-delivery retry budget spans only %v, want >= 30s — too short to "+
			"outlast the downstream blips the pacing exists to survive (base %v, cap %v)",
			deliveryLimit, budget.Round(time.Second), o.requeueBackoffBase, o.requeueBackoffMax)
	}
}
