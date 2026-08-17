package sequence

import (
	"log/slog"
	"testing"
	"time"

	"github.com/quarks-tech/protoevent-go/pkg/transport/outbox/internal/notify"
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
	// Deliberately NOT LeaseTTL: the failover budget and the store-call budget
	// are separate knobs, so lowering one cannot silently tighten the other.
	if o.OpTimeout != 30*time.Second {
		t.Fatalf("OpTimeout = %v, want 30s", o.OpTimeout)
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

func TestOptionsApply(t *testing.T) {
	o := defaultOptions()
	for _, opt := range []Option{
		WithBatchSize(50),
		WithSequenceBatchSize(500),
		WithoutSequencer(),
		WithRetention(5*time.Minute, 5000),
	} {
		opt(&o)
	}
	if o.BatchSize != 50 || o.SequenceBatchSize != 500 {
		t.Fatalf("batch sizes not applied: %+v", o)
	}
	if !o.SequencerDisabled {
		t.Fatal("WithoutSequencer did not set SequencerDisabled")
	}
	// WithRetention tunes CADENCE only — there is no per-relay window option
	// any more; how much history survives is the store's (see Sweeper).
	if !o.RetentionConfigured || o.RetentionSweepInterval != 5*time.Minute || o.RetentionSweepBatch != 5000 {
		t.Fatalf("retention cadence not applied: %+v", o)
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
