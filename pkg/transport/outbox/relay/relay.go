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
	// (messages parked via a PoisonHandler are reported through OnError and
	// not counted here), age of the oldest event handled (lag), and whether
	// more work is immediately waiting.
	// oldestAge is measured from the runtime's committed anchor: insert-time
	// (Message.CreateTime) age of the oldest event in the page for the
	// sequence runtime, and committed-token (commitTime) age for the stream
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
	// OnSwept reports how many fully-consumed rows a retention sweep pass
	// deleted. Fired by the sequence runtime only (the MongoDB runtime prunes
	// via a TTL index, not sweep passes). A sweep that repeatedly deletes
	// full batches is the falling-behind signal: deletable rows are
	// accumulating faster than the sweep cadence removes them.
	OnSwept func(name string, count int)
	// OnLeadership reports leadership transitions: fired once when this relay
	// instance becomes leader and once when it stops being leader (standby
	// takeover, lease loss mid-pass, graceful release). Without it a
	// dual-leader episode — a wedged leader resuming after a standby took
	// over — leaves no trace in either instance's telemetry; with it the
	// handover timeline is reconstructable. Not fired for steady-state
	// renewals.
	OnLeadership func(name string, isLeader bool)
}

// PoisonHandler is the poison-parking hook shared by both runtimes: called for
// a message whose PERSISTED PAYLOAD failed to decode (a typed DecodeError).
// Return nil ONLY once the message is durably parked (DLQ write committed,
// alert recorded — whatever "parked" means to you): a nil return authorizes
// the relay to advance its offset/token past the row, which is irreversible.
// A non-nil return stops the lane at the poison row instead — exactly as if
// no handler were configured — and the park is retried next pass, so a
// transient DLQ outage cannot silently skip an event forever.
//
// Send failures are never routed here: a send failure is downstream trouble,
// not a message fault, and the relay always stops the lane and retries the
// same message (order AND delivery preserved — the whole point of an outbox).
// Shutdown cancellation is never routed here either — a canceled run context
// stops the lane instead of parking healthy messages.
//
// STUB CONTRACT: msg may be a partial envelope. The poison path fires
// precisely because the persisted payload failed to decode, so the handler
// receives only what survived — ID (and Seq for the sequence runtime), with
// nil Metadata and Data. A handler that needs the raw poison bytes for
// forensics must fetch the row by ID from the store; err (unwrappable to the
// runtime's DecodeError) carries the position and decode failure.
type PoisonHandler func(ctx context.Context, msg *outbox.Message, err error) error

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
