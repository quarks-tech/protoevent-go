package stream

import (
	"log/slog"
	"testing"
	"time"
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
	if o.Observer == nil || o.Logger == nil {
		t.Fatal("default Observer/Logger must be non-nil (the runtime never nil-checks them)")
	}
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

func TestWithObserverNilIsNoop(t *testing.T) {
	o := defaultOptions()
	def := o.Observer
	WithObserver(nil)(&o)
	if o.Observer != def {
		t.Fatal("WithObserver(nil) replaced the default observer")
	}
}
