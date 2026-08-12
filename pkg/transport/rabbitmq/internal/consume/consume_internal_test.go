package consume

import (
	"strconv"
	"strings"
	"testing"
)

// TestCheckPrefetchRejectsNonPositive pins that an unbounded prefetch fails the
// subscription rather than being silently substituted.
//
// AMQP reads prefetch-count 0 as "no specified limit", and the receivers used to
// pass it straight to Channel.Qos, so WithPrefetchCount(0) was a working unlimited
// consumer. Drain-on-cancel cannot honor that — an unbounded prefetch means an
// unbounded number of buffered deliveries to finish inside DrainTimeout. Falling
// back to the default instead would leave a consumer running at a prefetch nobody
// asked for, which is a throughput mystery weeks later; the error names the option
// to fix, in the caller's own terms rather than amqpx's ConsumeSpec.
func TestCheckPrefetchRejectsNonPositive(t *testing.T) {
	for _, configured := range []int{0, -1} {
		t.Run(strconv.Itoa(configured), func(t *testing.T) {
			err := checkPrefetch("rabbitmq", configured)
			if err == nil {
				t.Fatalf("checkPrefetch(%d) = nil, want an error", configured)
			}
			if !strings.Contains(err.Error(), "WithPrefetchCount") {
				t.Fatalf("error = %v, want it to name the caller's own option", err)
			}
			if !strings.HasPrefix(err.Error(), "rabbitmq: ") {
				t.Fatalf("error = %v, want it prefixed with the calling runtime", err)
			}
		})
	}
}

// TestCheckPrefetchNamesTheCallingRuntime pins that the parking-lot receiver's
// error says "parkinglot", not "rabbitmq": the two are configured separately and
// an operator must be able to tell which subscription failed.
func TestCheckPrefetchNamesTheCallingRuntime(t *testing.T) {
	err := checkPrefetch("parkinglot", 0)
	if err == nil || !strings.HasPrefix(err.Error(), "parkinglot: ") {
		t.Fatalf("error = %v, want a parkinglot-prefixed error", err)
	}
}

// TestCheckPrefetchAcceptsPositive pins that the guard is not over-broad.
func TestCheckPrefetchAcceptsPositive(t *testing.T) {
	for _, configured := range []int{1, DefaultPrefetchCount, 50} {
		if err := checkPrefetch("rabbitmq", configured); err != nil {
			t.Fatalf("checkPrefetch(%d) = %v, want nil", configured, err)
		}
	}
}
