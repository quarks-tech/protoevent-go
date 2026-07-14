// Package relay holds the primitives shared by the sequenced-log (relay/sequence,
// TiDB) and change-stream (relay/stream, MongoDB) outbox relay runtimes.
package relay

import (
	"context"
	"log/slog"
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

// BoundContext returns the context for one relay store operation. While ctx is
// alive it is bounded by liveTimeout (0 = no bound, ctx returned as-is); once
// ctx is canceled — the final offset commit / token save / stream close on a
// planned shutdown — a fresh Background context bounded by deadTimeout
// replaces it, so those writes are still deliverable and bounded (a real store
// fails writes on a dead context, and losing the final commit redelivers
// acknowledged sends on restart). Always call the returned cancel.
func BoundContext(ctx context.Context, liveTimeout, deadTimeout time.Duration) (context.Context, context.CancelFunc) {
	if ctx.Err() != nil {
		return context.WithTimeout(context.Background(), deadTimeout)
	}
	if liveTimeout <= 0 {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, liveTimeout)
}

// HandleMessageError routes one failed message through the shared error
// policy: the ErrorHandler (nil for stop-the-lane failures — only poison
// DecodeErrors are ever parked), the Observer, and the Logger. Shared so the
// two runtimes cannot drift on park/observe/log semantics.
func HandleMessageError(ctx context.Context, h ErrorHandler, obs Observer, log *slog.Logger, runtime, name string, msg *outbox.Message, err error) {
	if h != nil {
		h(ctx, msg, err)
	}
	obs.ObserveError(name, err)
	log.Error(runtime+" relay: message failed", "relay", name, "event_id", msg.ID, "err", err)
}
