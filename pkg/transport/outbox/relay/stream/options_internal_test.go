package stream

import (
	"log/slog"
	"testing"
	"time"

	"github.com/quarks-tech/protoevent-go/pkg/transport/outbox/internal/relayutil"
)

// Internal tests for the unexported options: the struct is deliberately not
// part of the public surface (the With* constructors are the only
// configuration path), so its defaults are pinned here.

func TestDefaultOptions(t *testing.T) {
	o := defaultOptions()
	if o.DrainWindow != time.Second {
		t.Fatalf("DrainWindow = %v, want 1s", o.DrainWindow)
	}
	if o.LeaseTTL != 15*time.Second {
		t.Fatalf("LeaseTTL = %v, want 15s", o.LeaseTTL)
	}
	if o.TokenBatchSize != 100 {
		t.Fatalf("TokenBatchSize = %d, want 100", o.TokenBatchSize)
	}
	if o.Logger == nil {
		t.Fatal("default Logger must be non-nil (the runtime never nil-checks it)")
	}
	// The zero relay.Observer is the default: its nil-safe dispatch methods
	// discard every signal, so no non-nil default is needed.
	relayutil.ObserveDrained(o.Observer, "c", 1, 0, false)
	relayutil.ObserveError(o.Observer, "c", nil)
	relayutil.ObserveSequenced(o.Observer, "c", 1)
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
