package sequence_test

import (
	"testing"
	"time"

	"github.com/quarks-tech/protoevent-go/pkg/transport/outbox/relay/sequence"
)

func TestDefaultOptions(t *testing.T) {
	o := sequence.DefaultOptions()
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
}

func TestOptionsApply(t *testing.T) {
	o := sequence.DefaultOptions()
	for _, opt := range []sequence.Option{
		sequence.WithBatchSize(50),
		sequence.WithSequenceBatchSize(500),
		sequence.WithoutSequencer(),
		sequence.WithRetention(7*24*time.Hour, 256, 5000),
	} {
		opt(&o)
	}
	if o.BatchSize != 50 || o.SequenceBatchSize != 500 {
		t.Fatalf("batch sizes not applied: %+v", o)
	}
	if !o.DisableSequencer {
		t.Fatal("WithoutSequencer did not set DisableSequencer")
	}
	if o.RetentionWindow != 7*24*time.Hour || o.RetentionSweepEvery != 256 || o.RetentionSweepBatch != 5000 {
		t.Fatalf("retention not applied: %+v", o)
	}
}
