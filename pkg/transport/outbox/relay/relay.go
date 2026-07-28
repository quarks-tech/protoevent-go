// Package relay holds the primitives shared by the sequenced-log (relay/sequence,
// TiDB) and change-stream (relay/stream, MongoDB) outbox relay runtimes.
package relay

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
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

// ErrPartialLeaderStore reports a store that implements SOME of LeaderStore but
// not all of it. Both runtimes discover leadership support with a runtime type
// assertion whose miss is a documented single-instance mode (always-leader), so
// a store that MEANT to be a LeaderStore — one method renamed, a signature
// drifted, a method added to the interface after the store was written — would
// otherwise degrade to always-leader silently and run dual leaders across
// replicas. A half-implemented capability is a bug, not a mode: it is rejected
// at construction.
var ErrPartialLeaderStore = errors.New("relay: store partially implements relay.LeaderStore")

// CheckLeaderStore resolves a store's leadership capability. It returns the
// LeaderStore when the store implements the whole interface, (nil, nil) when it
// has no trace of it (always-leader single-instance mode), and an error wrapping
// ErrPartialLeaderStore when it looks like a LeaderStore but is not one.
//
// The probe is by method NAME, via reflection, deliberately. Probing with narrower
// interfaces catches only the store that lost exactly one method: one where BOTH
// signatures drifted — a refactor to a config-struct parameter, a rename, or the
// interface itself growing — matches no probe at all and falls straight through to
// the always-leader mode this check exists to prevent. A type carrying either
// method name has clearly been written to be a LeaderStore, so failing to satisfy
// the interface is a bug in it, not a request for single-instance mode.
func CheckLeaderStore(store any) (LeaderStore, error) {
	if ls, ok := store.(LeaderStore); ok {
		return ls, nil
	}

	present := leaderMethodNames(store)
	if len(present) == 0 {
		return nil, nil // no trace of the capability: single-instance mode
	}

	return nil, fmt.Errorf("%w: has %s but does not satisfy relay.LeaderStore"+
		" — check the method set against"+
		" TryAcquireLeaderLock(ctx context.Context, name, holderID string, ttl time.Duration) (bool, error) and"+
		" ReleaseLeaderLock(ctx context.Context, name, holderID string) error"+
		" (a store that cannot conditionally delete an owner-checked lock row may implement Release as a no-op returning nil)",
		ErrPartialLeaderStore, strings.Join(present, " and "))
}

// leaderMethodNames returns the LeaderStore method names the store defines,
// whatever their signatures.
//
// It checks the POINTER type's method set as well as the value's. A value type's
// method set excludes pointer-receiver methods, so a store passed by value whose
// lock methods take a pointer receiver would satisfy neither the interface nor a
// value-only name probe — and fall through to always-leader, which is the outcome
// this whole check exists to prevent. (Such a store cannot work as a LeaderStore
// either way; the point is that it must fail loudly rather than silently.)
func leaderMethodNames(store any) []string {
	if store == nil {
		return nil
	}

	t := reflect.TypeOf(store)

	has := func(name string) bool {
		if _, ok := t.MethodByName(name); ok {
			return true
		}
		if t.Kind() != reflect.Pointer {
			if _, ok := reflect.PointerTo(t).MethodByName(name); ok {
				return true
			}
		}

		return false
	}

	var present []string
	for _, name := range []string{"TryAcquireLeaderLock", "ReleaseLeaderLock"} {
		if has(name) {
			present = append(present, name)
		}
	}

	return present
}
