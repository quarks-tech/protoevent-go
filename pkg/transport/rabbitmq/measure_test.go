package rabbitmq_test

import (
	"os"
	"testing"
)

// requireMeasure gates the throughput/latency measurement harnesses. They are
// minutes long by construction — one of them waits out a 15s-capped backoff
// ladder — so they are opt-in rather than part of `make test`:
//
//	OUTBOX_MEASURE=1 go test ./pkg/transport/rabbitmq -run TestOnePoison -v
func requireMeasure(t *testing.T) {
	t.Helper()
	if os.Getenv("OUTBOX_MEASURE") == "" {
		t.Skip("measurement harness: set OUTBOX_MEASURE=1 to run")
	}
}
