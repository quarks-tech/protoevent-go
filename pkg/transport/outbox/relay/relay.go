// Package relay holds the primitives shared by the sequenced-log (relay/sequence,
// TiDB) and change-stream (relay/stream, MongoDB) outbox relay runtimes.
package relay

import (
	"context"
	"time"

	"github.com/quarks-tech/protoevent-go/pkg/transport/outbox"
)

// Observer receives lag/throughput signals common to every relay runtime. It
// is the dependency-free observability seam: callers wire it to Prometheus
// etc. Values are derived from data the relay's passes already hold — no
// extra queries.
//
// Observer is a struct of nil-able callbacks (httptrace.ClientTrace style),
// not an interface: set only the signals you care about — a nil callback is
// simply never invoked — and future signals are additive rather than
// breaking. The zero value discards everything. Both runtimes accept the
// same type; OnSequenced simply never fires for the stream runtime.
type Observer struct {
	// OnDrained reports a drain/forward pass: count successfully sent
	// (messages parked via an ErrorHandler are reported through OnError and
	// not counted here), age of the oldest event handled (lag), and whether
	// more work is immediately waiting.
	// oldestAge is measured from the runtime's committed anchor: insert-time
	// (Message.CreateTime) age of the oldest event in the page for the
	// sequence runtime, and committed-token (clusterTime) age for the stream
	// runtime.
	//
	// more reports whether work is known to remain: a full page, or a
	// stop-the-lane failure (the failed event is still pending).
	OnDrained func(name string, count int, oldestAge time.Duration, more bool)
	// OnError reports a pass-level error (leadership, read, forward, sweep)
	// or a per-message failure (send failure, parked poison row).
	OnError func(name string, err error)
	// OnSequenced reports how many rows a sequencer pass assigned. Fired by
	// the sequence runtime only.
	OnSequenced func(name string, count int)
}

// ErrorHandler is the poison-parking hook shared by both runtimes: called for
// a message whose PERSISTED PAYLOAD failed to decode (a typed DecodeError),
// after which the relay advances past it — a poison row can never succeed on
// retry, so parking it is the only way to keep the lane moving. Send failures
// are never routed here: a send failure is downstream trouble, not a message
// fault, and the relay always stops the lane and retries the same message
// (order AND delivery preserved — the whole point of an outbox). Shutdown
// cancellation is never routed here either — a canceled run context stops the
// lane instead of parking healthy messages.
type ErrorHandler func(ctx context.Context, msg *outbox.Message, err error)

// LeaderStore enables running multiple relay instances with automatic failover.
// Only the lock holder processes; others idle. Shared by both runtimes.
type LeaderStore interface {
	// TryAcquireLeaderLock acquires or renews the lock. Returns true if holderID
	// holds it after the call. The lock expires after ttl if not renewed.
	//
	// Expiry MUST be evaluated against a single authoritative clock — the
	// store's own (DB server) clock, never the caller's wall clock: with
	// client-side time.Now() a standby with a fast clock steals a live lease
	// (dual leader) under clock skew between relay instances. Both reference
	// implementations do this (TiDB NOW(6), MongoDB $$NOW).
	TryAcquireLeaderLock(ctx context.Context, name, holderID string, ttl time.Duration) (bool, error)
	// ReleaseLeaderLock releases the lock if held by holderID (graceful shutdown).
	ReleaseLeaderLock(ctx context.Context, name, holderID string) error
}
