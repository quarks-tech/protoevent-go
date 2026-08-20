package event_test

import (
	"errors"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/quarks-tech/protoevent-go/pkg/event"
)

// TestValidExtensionValueRangeChecksPlainInt pins the one accepted type whose
// Go width and AMQP width disagree.
//
// amqp091-go encodes Go's `int` as a 32-bit signed field ('I'), so on a 64-bit
// host a value above MaxInt32 is truncated on the wire while the validator
// reports it safe — WithEventExtension("account", 3_000_000_000) arriving as
// -1294967296 with nothing anywhere saying so. An untyped constant defaults to
// `int`, which makes this the DEFAULT way to write a large integer extension,
// not an exotic one.
//
// int64 stays accepted at any magnitude: the encoder writes it as a 64-bit field
// ('l'), so nothing is lost, and the error message points there.
func TestValidExtensionValueRangeChecksPlainInt(t *testing.T) {
	t.Run("in-range int is accepted", func(t *testing.T) {
		for _, v := range []int{0, -1, math.MaxInt32, math.MinInt32} {
			if err := event.ValidExtensionValue(v); err != nil {
				t.Fatalf("ValidExtensionValue(%d) = %v, want nil", v, err)
			}
		}
	})

	t.Run("out-of-range int is rejected", func(t *testing.T) {
		for _, v := range []int{math.MaxInt32 + 1, math.MinInt32 - 1} {
			err := event.ValidExtensionValue(v)
			if err == nil {
				t.Fatalf("ValidExtensionValue(%d) = nil, want an error: the value is silently "+
					"truncated to 32 bits on the wire", v)
			}
			if !strings.Contains(err.Error(), "int64") {
				t.Fatalf("error %q does not name int64, the type that does survive", err)
			}
		}
	})

	t.Run("int64 is accepted at any magnitude", func(t *testing.T) {
		if err := event.ValidExtensionValue(int64(math.MaxInt64)); err != nil {
			t.Fatalf("ValidExtensionValue(int64 max) = %v, want nil (AMQP writes int64 as a 64-bit field)", err)
		}
	})

	t.Run("the rest of the accepted set is unchanged", func(t *testing.T) {
		for _, v := range []any{
			"s", true, int8(1), int16(1), int32(1), uint8(1), uint16(1), uint32(1),
			float32(1), float64(1), []byte("b"), time.Now(),
		} {
			if err := event.ValidExtensionValue(v); err != nil {
				t.Fatalf("ValidExtensionValue(%T) = %v, want nil", v, err)
			}
		}
	})
}

// TestSplitTypeErrorIsUnsendable pins the marker a relay needs to get PAST a
// persisted row it can never send.
//
// A dot-less event type is a property of the value: the RabbitMQ sender needs
// the dot to split exchange from routing key, so no retry can ever succeed.
// Publish-time validation keeps such a type out of the store, but an outbox row
// is durable and rows predating that check still arrive at the sender — where an
// unmarked error is claimed by no classifier and stops the lane on that row every
// tick forever.
func TestSplitTypeErrorIsUnsendable(t *testing.T) {
	_, _, err := event.SplitType("nodots")
	if err == nil {
		t.Fatal("SplitType(\"nodots\") = nil error, want a malformed-type error")
	}
	if !errors.Is(err, event.ErrUnsendable) {
		t.Fatalf("SplitType error %v does not wrap event.ErrUnsendable; a relay cannot tell it "+
			"apart from downstream trouble and will retry the row forever", err)
	}

	if _, _, err := event.SplitType("svc.Event"); err != nil {
		t.Fatalf("SplitType(\"svc.Event\") = %v, want nil", err)
	}
}
