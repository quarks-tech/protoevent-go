package stream_test

import (
	"testing"
	"time"

	"github.com/quarks-tech/protoevent-go/pkg/transport/outbox/relay/stream"
)

func TestDefaultOptions(t *testing.T) {
	o := stream.DefaultOptions()
	if o.DrainWindow != time.Second {
		t.Fatalf("DrainWindow = %v, want 1s", o.DrainWindow)
	}
	if o.LeaseTTL != 15*time.Second {
		t.Fatalf("LeaseTTL = %v, want 15s", o.LeaseTTL)
	}
	if o.TokenBatchSize != 100 {
		t.Fatalf("TokenBatchSize = %d, want 100", o.TokenBatchSize)
	}
}

func TestNewRelayRejectsDrainWindowTooLarge(t *testing.T) {
	// DrainWindow must be < LeaseTTL/2 so the lease can be renewed within a window.
	_, err := stream.NewRelay("c", nil, nil,
		stream.WithLeaseTTL(10*time.Second), stream.WithDrainWindow(6*time.Second))
	if err == nil {
		t.Fatal("expected error for DrainWindow >= LeaseTTL/2, got nil")
	}
}

func TestNewRelayAcceptsValidWindow(t *testing.T) {
	r, err := stream.NewRelay("c", nil, nil,
		stream.WithLeaseTTL(10*time.Second), stream.WithDrainWindow(1*time.Second))
	if err != nil || r == nil {
		t.Fatalf("NewRelay valid config: r=%v err=%v", r, err)
	}
}
