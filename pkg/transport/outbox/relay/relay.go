// Package relay holds the primitives shared by the sequenced-log (relay/sequence,
// TiDB) and change-stream (relay/stream, MongoDB) outbox relay runtimes.
package relay

import (
	"context"
	"fmt"
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
//
// Callbacks MUST NOT BLOCK. They run on the relay's single pass goroutine, which is
// also the goroutine that reads the store, sends to the broker and renews the leader
// lease — so a callback that waits on a lock, a network write or a full channel stops
// delivery for as long as it waits. Nothing bounds them: unlike the store capabilities,
// the send and the PoisonHandler, which each carry an OpTimeout budget, an observer
// callback is assumed cheap because its whole purpose is to hand a number to a metrics
// registry. A blocked one is also invisible, because the signals that would report a
// stalled relay are these same callbacks. Record the value and return; do the work
// elsewhere. (gochan.WithErrorHandler states the same requirement for the same reason.)
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
	//
	// Fired for a zero count too, which is NOT redundant: the sweep's cutoff is
	// MIN(last_seq) across all consumer groups, so a single lagging or replaying
	// group pins it and blocks pruning store-wide. A blocked sweep and a healthy
	// idle one both delete nothing, so a signal that only fired for n > 0 left
	// them indistinguishable while the log grew toward disk-full. Alert on
	// "swept 0 while the table keeps growing", not on the count alone.
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
// Send failures are not routed here by default: a send failure is downstream
// trouble, not a message fault, and the relay stops the lane and retries the
// same message (order AND delivery preserved — the whole point of an outbox).
// The one exception is opt-in: with an UnsendableClassifier configured, a
// failure that classifier calls permanent for that specific message is parked
// here too, so a message the broker will never accept cannot wedge the log
// forever. Shutdown cancellation is never routed here — a canceled run context
// stops the lane instead of parking healthy messages.
//
// STUB CONTRACT: msg may be a partial envelope. The poison path fires
// precisely because the persisted payload failed to decode, so the handler
// receives only what survived — ID (and Seq for the sequence runtime), with
// nil Metadata and Data. A handler that needs the raw poison bytes for
// forensics must fetch the row by ID from the store; err (unwrappable to the
// runtime's DecodeError) carries the position and decode failure.
type PoisonHandler func(ctx context.Context, msg *outbox.Message, err error) error

// UnsendableClassifier reports whether a Sender.Send failure is PERMANENT for
// this specific message — the message will never be accepted by the downstream
// transport, however many times it is retried (a body over the broker's
// frame_max, a rejected routing key, a payload the sender cannot marshal).
//
// It exists because stopping the lane on every send failure — right for an
// outage, since order and delivery are the point of an outbox — is a trap for a
// message the broker will refuse forever: that one row wedges the log at its
// head and every event behind it stops being delivered, recoverable only by
// hand-editing offsets in a live database. A classifier converts exactly that
// case into a park: the message goes to the PoisonHandler and the relay advances
// past it, while everything the classifier does NOT claim keeps the
// stop-the-lane behavior.
//
// Return false unless the error is genuinely terminal for this message. A
// classifier that claims transient errors (broker down, timeout, closed
// connection) bulk-diverts the whole backlog to the DLQ during an outage — the
// failure mode stop-the-lane exists to prevent. Errors are broker-specific, so
// there is no safe default: without a classifier every send failure stops the
// lane, as before.
//
// A classifier requires a PoisonHandler (there is nowhere to park otherwise);
// both runtimes reject the combination at construction.
type UnsendableClassifier func(err error) bool

// LeaderStore enables running multiple relay instances with automatic failover.
// Only the lock holder processes; others idle. Shared by both runtimes.
//
// Each runtime's NewRelay discovers this capability with a plain type assertion,
// and a store that does not satisfy it is a construction ERROR unless the caller
// waives election with WithoutLeaderElection.
//
// That used to be a soft check — a miss meant always-leader single-instance mode
// — backed by a reflective probe for the two method NAMES, so a store that meant
// to elect but had drifted would fail loudly instead of silently running dual
// leaders. The probe could not see a drift that kept both names (the likely one:
// a signature change), so it never closed the hole it was written for. An
// explicit waiver does: absence is now either declared or an error, and no store
// can be silently downgraded to always-leader.
type LeaderStore interface {
	// TryAcquireLeaderLock acquires or renews the lock. Returns true if holderID
	// holds it after the call. The lock expires after ttl if not renewed.
	//
	// Expiry MUST be evaluated against ONE clock, shared by every instance of the
	// group, and the deadline MUST be stored in a form that cannot be read in a
	// different frame of reference than it was written in. Two instances that
	// disagree about the current time by more than ttl can both consider themselves
	// leader.
	//
	// A server-side clock is the easy way to satisfy this — one clock serves every
	// instance — and MongoDB does it with $$NOW, which is safe because a BSON date is
	// an unambiguous UTC instant.
	//
	// TiDB does NOT: it sends the deadline and the comparison from the relay process
	// as bound parameters. NOW(6) looked like a single authoritative clock and was
	// not one — it renders in the SESSION's time_zone and lands in a zone-less
	// DATETIME(6) column, so a session eight hours ahead read every live lease as
	// long expired and stole it outright, while a session behind never took over a
	// lease that had genuinely expired. Trading that for the relay's own clock costs
	// the single-clock property and buys away an error that needed no clock skew at
	// all, only a differing time_zone.
	//
	// The consequence is that leadership alone is NOT sufficient to protect the log:
	// a relay whose clock runs fast can acquire a lease another instance still holds.
	// Ordering therefore has to be defended where the damage would happen — the
	// offset commit must be fenced by holder, not merely monotone — rather than
	// resting on the lease being unstealable.
	TryAcquireLeaderLock(ctx context.Context, name, holderID string, ttl time.Duration) (bool, error)
	// ReleaseLeaderLock releases the lock if held by holderID, so a standby
	// takes over immediately on graceful shutdown instead of waiting out the
	// lease. Release is a courtesy, not a correctness requirement: a store
	// that cannot conditionally delete an owner-checked lock row may
	// implement it as a no-op — failover then degrades to at most one lease
	// TTL of delay.
	ReleaseLeaderLock(ctx context.Context, name, holderID string) error
}

// StuckLaneError reports a lane that has been stopped on the SAME message for
// longer than the runtime's escalation threshold — i.e. the log is wedged and
// nothing behind that message is being delivered.
//
// It exists because stopping the lane on a failure is right for an outage but,
// tick by tick, indistinguishable from a message the downstream will NEVER accept.
// In the second case every event behind it stops being delivered indefinitely and
// recovery means editing the persisted position by hand, yet the per-tick OnError
// says only "a send failed" — the same thing a two-minute broker blip says. This
// reaches Observer.OnError ONCE per stuck episode, so an alert can tell "having a
// bad minute" from "needs a human".
//
// The relay does not act on it: skipping the message would break the delivery
// guarantee on a guess. See each runtime's WithUnsendableClassifier for the
// configured way to park such a message automatically.
//
// Unwrap returns the underlying failure.
type StuckLaneError struct {
	// Position identifies where the lane is stuck, in the runtime's own terms:
	// the seq for the sequenced-log runtime, the resume token for the change-stream
	// runtime.
	Position string
	ID       string        // event_id of the message the lane is stopped on, when known
	StuckFor time.Duration // how long the lane has been stopped on it
	Err      error         // the underlying failure (send failure, or a failed park)
}

func (e *StuckLaneError) Error() string {
	return fmt.Sprintf("relay: lane wedged at %s (event %s) for %s: %v", e.Position, e.ID, e.StuckFor, e.Err)
}

func (e *StuckLaneError) Unwrap() error { return e.Err }
