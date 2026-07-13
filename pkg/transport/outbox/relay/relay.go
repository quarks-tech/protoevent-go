// Package relay holds the primitives shared by the sequenced-log (relay/sequence,
// TiDB) and change-stream (relay/stream, MongoDB) outbox relay runtimes.
package relay

import (
	"context"
	"time"

	"github.com/quarks-tech/protoevent-go/pkg/transport/outbox"
)

// Observer receives lag/throughput signals common to every relay runtime. It is
// the dependency-free observability seam: callers wire it to Prometheus etc.
// Values are derived from data the relay's passes already hold — no extra queries.
type Observer interface {
	// ObserveDrained reports a drain/forward pass: count successfully sent
	// (messages parked via an ErrorHandler are reported through ObserveError
	// and not counted here), age of the oldest event handled (lag), and
	// whether more work is immediately waiting.
	// oldestAge is measured from the runtime's committed anchor: insert-time
	// (Message.CreateTime) age of the oldest event in the page for the
	// sequence runtime, and committed-token (clusterTime) age for the stream
	// runtime.
	//
	// more reports whether work is known to remain: a full page, or a
	// stop-the-lane failure (the failed event is still pending).
	ObserveDrained(name string, count int, oldestAge time.Duration, more bool)
	// ObserveError reports a pass-level error (leadership, read, forward, sweep).
	ObserveError(name string, err error)
}

// ErrorHandler is the park-and-continue hook shared by both runtimes: called
// for a message that failed to send (or decode), after which the relay
// advances past it. Configuring one trades per-event ordering for liveness.
// Shutdown cancellation is never routed here — a canceled run context stops
// the lane instead of parking healthy messages.
type ErrorHandler func(ctx context.Context, msg *outbox.Message, err error)

// NopObserver returns an Observer that discards all signals — the default
// when no observer is wired.
func NopObserver() Observer { return nopObserver{} }

type nopObserver struct{}

func (nopObserver) ObserveDrained(string, int, time.Duration, bool) {}
func (nopObserver) ObserveError(string, error)                      {}

// LeaderStore enables running multiple relay instances with automatic failover.
// Only the lock holder processes; others idle. Shared by both runtimes.
type LeaderStore interface {
	// TryAcquireLeaderLock acquires or renews the lock. Returns true if holderID
	// holds it after the call. The lock expires after ttl if not renewed.
	TryAcquireLeaderLock(ctx context.Context, name, holderID string, ttl time.Duration) (bool, error)
	// ReleaseLeaderLock releases the lock if held by holderID (graceful shutdown).
	ReleaseLeaderLock(ctx context.Context, name, holderID string) error
}
