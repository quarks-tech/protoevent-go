package parkinglot

import (
	"context"
	"errors"
	"testing"

	amqp "github.com/rabbitmq/amqp091-go"

	"github.com/quarks-tech/amqpx/connpool"
)

// TestDefaultReceiverLoggerIsNotNil pins the default this receiver needs even
// more than the plain one: the branch reporting a failed d.Ack after a DURABLE
// park is log-and-continue by design (returning would requeue and re-park), so
// a nil logger left the delivery unacked, holding a QoS slot, with no error, no
// log and no metric. The unreadable-x-death branch is the same shape.
func TestDefaultReceiverLoggerIsNotNil(t *testing.T) {
	if defaultReceiverOptions().logger == nil {
		t.Fatal("default logger is nil: a failed ack after a successful park would be silent")
	}
}

func TestWithLoggerNilKeepsTheDefault(t *testing.T) {
	o := defaultReceiverOptions()
	WithLogger(nil)(&o)

	if o.logger == nil {
		t.Fatal("WithLogger(nil) cleared the default logger")
	}
}

// TestPutIntoParkingLotDetachesCanceledContext pins the drain-mode contract:
// under amqpx ProcessWithDrain the command ctx IS the canceled shutdown ctx,
// and amqp091's PublishWithContext fast-path rejects a canceled ctx before
// touching the channel. putIntoParkingLot must detach (context.WithoutCancel)
// so the publish is actually attempted during drain. With a zero-value
// channel, an ATTEMPTED publish panics — which is exactly the evidence the
// fast-path was bypassed; a context.Canceled error return means the detach
// regressed.
func TestPutIntoParkingLotDetachesCanceledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	// The channel is pre-marked as already in confirm mode so enableConfirms
	// short-circuits: otherwise ch.Confirm panics on the zero-value channel
	// before the ctx ever reaches the publish, and the assertion below would pass
	// for the wrong reason.
	ch := &amqp.Channel{}
	receiver := &Receiver{
		options:    receiverOptions{dlxExchange: "events.dlx"},
		confirming: map[*amqp.Channel]struct{}{ch: {}},
	}
	conn := connpool.NewConn(nil, ch)

	var err error
	panicked := func() (panicked bool) {
		defer func() { panicked = recover() != nil }()
		err = receiver.putIntoParkingLot(ctx, conn, &amqp.Delivery{})
		return false
	}()

	if errors.Is(err, context.Canceled) {
		t.Fatalf("putIntoParkingLot() error = %v: canceled ctx reached the publish — WithoutCancel detach regressed", err)
	}
	if !panicked {
		t.Fatalf("expected the zero-value channel publish to be attempted (panic); got err = %v", err)
	}
}

// TestHasExceededRetryCountAcceptsAnyIntegerEncoding pins the retry-budget
// evaluation against x-death `count` encodings other than AMQP long. RabbitMQ
// itself sends int64, but proxies, shovels and federation links have been seen
// re-encoding it narrower. A missed assertion used to mean "limit not reached",
// so doAcknowledge rejected instead of parking and the poison delivery looped
// DLX → wait → TTL → incoming forever, re-running the handler's side effects
// every lap with nothing logged.
func TestHasExceededRetryCountAcceptsAnyIntegerEncoding(t *testing.T) {
	counts := map[string]any{
		"int64":  int64(5),
		"int32":  int32(5),
		"int16":  int16(5),
		"int8":   int8(5),
		"int":    5,
		"uint64": uint64(5),
		"uint32": uint32(5),
		"uint16": uint16(5),
		"uint8":  uint8(5),
	}

	for name, count := range counts {
		t.Run(name, func(t *testing.T) {
			d := deliveryWithDeathCount(count)
			if !receiverWithMaxRetries(5).hasExceededRetryCount(d) {
				t.Fatalf("hasExceededRetryCount(count=%v (%s), max=5) = false, want true", count, name)
			}
			if receiverWithMaxRetries(6).hasExceededRetryCount(d) {
				t.Fatalf("hasExceededRetryCount(count=%v (%s), max=6) = true, want false", count, name)
			}
		})
	}
}

// TestHasExceededRetryCountKeepsRetryingUnreadableCount pins the bias for a count
// no conversion handles. hasExceededRetryCount is consulted for TRANSIENT handler
// failures only, so parking on an unreadable count would cut the retry budget to
// a single lap: during any downstream blip, healthy events would be dead-lettered
// to the parking lot after one retry and need manual reprocessing. Keeping the
// retries is the safe direction; the receiver logs so the re-encoding is still
// diagnosable.
func TestHasExceededRetryCountKeepsRetryingUnreadableCount(t *testing.T) {
	unreadable := []any{"not-a-number", 1.5, true, nil, struct{}{}}

	for _, count := range unreadable {
		d := deliveryWithDeathCount(count)
		if receiverWithMaxRetries(5).hasExceededRetryCount(d) {
			t.Fatalf("hasExceededRetryCount(count=%#v) = true, want false (an unreadable count must not cap retries at one lap)", count)
		}
	}
}

// TestHasExceededRetryCountSkipsMalformedEntries pins that a non-Table entry
// ahead of the matching one does not abort the scan.
func TestHasExceededRetryCountSkipsMalformedEntries(t *testing.T) {
	d := &amqp.Delivery{
		Headers: amqp.Table{
			"x-first-death-queue": "events.incoming",
			"x-death": []any{
				"garbage",
				amqp.Table{"queue": "events.other", "count": int64(99)},
				amqp.Table{"queue": "events.incoming", "count": int64(5)},
			},
		},
	}
	if !receiverWithMaxRetries(5).hasExceededRetryCount(d) {
		t.Fatal("hasExceededRetryCount() = false, want true (the matching entry follows a malformed one)")
	}
}

// TestHasExceededRetryCountToleratesByteHeaders pins that AMQP longstr fields
// decoded as []byte do not panic the consumer. Comparing two []byte values through
// an interface panics with "comparing uncomparable type []uint8", and it would do
// so inside the delivery goroutine on the retry path — the one that is meant to be
// the safe one.
func TestHasExceededRetryCountToleratesByteHeaders(t *testing.T) {
	d := &amqp.Delivery{
		Headers: amqp.Table{
			"x-first-death-queue": []byte("events.incoming"),
			"x-death": []any{
				amqp.Table{"queue": []byte("events.incoming"), "count": int64(5)},
			},
		},
	}

	if !receiverWithMaxRetries(5).hasExceededRetryCount(d) {
		t.Fatal("hasExceededRetryCount() = false for []byte-encoded headers, want true")
	}

	// Mixed encodings across the two fields must still match.
	mixed := &amqp.Delivery{
		Headers: amqp.Table{
			"x-first-death-queue": "events.incoming",
			"x-death": []any{
				amqp.Table{"queue": []byte("events.incoming"), "count": int64(5)},
			},
		},
	}
	if !receiverWithMaxRetries(5).hasExceededRetryCount(mixed) {
		t.Fatal("hasExceededRetryCount() = false for mixed string/[]byte headers, want true")
	}
}

// TestHasExceededRetryCountRequiresFirstDeathQueue pins that an absent
// first-death queue matches no entry: string normalization turns two absent
// fields into two empty strings, which would otherwise compare equal and let a
// malformed entry stand in for the real one.
func TestHasExceededRetryCountRequiresFirstDeathQueue(t *testing.T) {
	d := &amqp.Delivery{
		Headers: amqp.Table{
			"x-death": []any{
				amqp.Table{"count": int64(99)},
			},
		},
	}

	if receiverWithMaxRetries(5).hasExceededRetryCount(d) {
		t.Fatal("hasExceededRetryCount() = true with no x-first-death-queue, want false")
	}
}

func receiverWithMaxRetries(max int64) *Receiver {
	return &Receiver{options: receiverOptions{maxRetries: max}}
}

func deliveryWithDeathCount(count any) *amqp.Delivery {
	return &amqp.Delivery{
		Headers: amqp.Table{
			"x-first-death-queue": "events.incoming",
			"x-death": []any{
				amqp.Table{"queue": "events.incoming", "count": count},
			},
		},
	}
}
