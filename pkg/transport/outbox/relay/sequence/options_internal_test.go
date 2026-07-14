package sequence

import (
	"log/slog"
	"testing"
	"time"
)

// Internal tests for the unexported options: the struct is deliberately not
// part of the public surface (the With* constructors are the only
// configuration path), so its defaults and setters are pinned here.

func TestDefaultOptions(t *testing.T) {
	o := defaultOptions()
	if o.BatchSize != 100 {
		t.Fatalf("BatchSize = %d, want 100", o.BatchSize)
	}
	if o.SequenceBatchSize != 1000 {
		t.Fatalf("SequenceBatchSize = %d, want 1000", o.SequenceBatchSize)
	}
	if o.PollInterval != time.Second {
		t.Fatalf("PollInterval = %v, want 1s", o.PollInterval)
	}
	if o.LeaseTTL != 15*time.Second {
		t.Fatalf("LeaseTTL = %v, want 15s", o.LeaseTTL)
	}
	if o.Observer == nil || o.Logger == nil {
		t.Fatal("default Observer/Logger must be non-nil (the runtime never nil-checks them)")
	}
}

func TestOptionsApply(t *testing.T) {
	o := defaultOptions()
	for _, opt := range []Option{
		WithBatchSize(50),
		WithSequenceBatchSize(500),
		WithoutSequencer(),
		WithRetention(7*24*time.Hour, 5*time.Minute, 5000),
	} {
		opt(&o)
	}
	if o.BatchSize != 50 || o.SequenceBatchSize != 500 {
		t.Fatalf("batch sizes not applied: %+v", o)
	}
	if !o.SequencerDisabled {
		t.Fatal("WithoutSequencer did not set SequencerDisabled")
	}
	if o.RetentionWindow != 7*24*time.Hour || o.RetentionSweepInterval != 5*time.Minute || o.RetentionSweepBatch != 5000 {
		t.Fatalf("retention not applied: %+v", o)
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
