package stream

import (
	"log/slog"
	"testing"
	"time"

	"github.com/quarks-tech/protoevent-go/pkg/transport/outbox/internal/notify"
)

// Internal tests for the unexported options: the struct is deliberately not
// part of the public surface (the With* constructors are the only
// configuration path), so its defaults are pinned here.

func TestDefaultOptions(t *testing.T) {
	o := defaultOptions()
	if o.DrainWindow != time.Second {
		t.Fatalf("DrainWindow = %v, want 1s", o.DrainWindow)
	}
	// 60s, not 15s: the lease must be able to CONTAIN a pass, and a pass is the
	// tick plus one store call. At 15s against the 30s OpTimeout below, one slow
	// store call returned onto an EXPIRED lease and the relay committed anyway.
	if o.LeaseTTL != 60*time.Second {
		t.Fatalf("LeaseTTL = %v, want 60s", o.LeaseTTL)
	}
	// Deliberately NOT LeaseTTL: the failover budget and the store-call budget
	// are separate knobs, so lowering one cannot silently tighten the other. They
	// are not unrelated, though — tick + OpTimeout must stay under LeaseTTL.
	if o.OpTimeout != 30*time.Second {
		t.Fatalf("OpTimeout = %v, want 30s", o.OpTimeout)
	}
	if o.TokenBatchSize != 100 {
		t.Fatalf("TokenBatchSize = %d, want 100", o.TokenBatchSize)
	}
	if o.Logger == nil {
		t.Fatal("default Logger must be non-nil (the runtime never nil-checks it)")
	}
	// The zero relay.Observer is the default: its nil-safe dispatch methods
	// discard every signal, so no non-nil default is needed.
	notify.Drained(o.Observer, "c", 1, 0, false)
	notify.Error(o.Observer, "c", nil)
	notify.Sequenced(o.Observer, "c", 1)
}

func TestWithLoggerNilIsNoop(t *testing.T) {
	o := defaultOptions()
	real := slog.New(slog.DiscardHandler)
	WithLogger(real)(&o)
	WithLogger(nil)(&o)
	if o.Logger != real {
		t.Fatal("WithLogger(nil) replaced a previously set logger")
	}
}
