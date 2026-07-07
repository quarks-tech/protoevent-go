// Package relay holds the primitives shared by the sequenced-log (relay/sequence,
// TiDB) and change-stream (relay/stream, MongoDB) outbox relay runtimes.
package relay

import (
	"context"
	"time"
)

// Observer receives lag/throughput signals common to every relay runtime. It is
// the dependency-free observability seam: callers wire it to Prometheus etc.
// Values are derived from data the relay passes already hold — no extra queries.
type Observer interface {
	// ObserveDrained reports a drain/forward pass: count sent, age of the oldest
	// event handled (lag), and whether more work is immediately waiting.
	ObserveDrained(name string, count int, oldestAge time.Duration, more bool)
	// ObserveError reports a pass-level error (leadership, read, forward, sweep).
	ObserveError(name string, err error)
}

// Logger interface for relay error logging.
type Logger interface {
	Errorf(format string, args ...any)
}

// LeaderStore enables running multiple relay instances with automatic failover.
// Only the lock holder processes; others idle. Shared by both runtimes.
type LeaderStore interface {
	// TryAcquireLeaderLock acquires or renews the lock. Returns true if holderID
	// holds it after the call. The lock expires after ttl if not renewed.
	TryAcquireLeaderLock(ctx context.Context, name, holderID string, ttl time.Duration) (bool, error)
	// ReleaseLeaderLock releases the lock if held by holderID (graceful shutdown).
	ReleaseLeaderLock(ctx context.Context, name, holderID string) error
}
