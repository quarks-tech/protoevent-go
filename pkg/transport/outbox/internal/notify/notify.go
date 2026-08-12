// Package notify delivers relay runtime signals to the user-configured hooks
// (Observer callbacks, PoisonHandler, Logger), nil-safely. It is the single
// dispatch path shared by the relay/sequence and relay/stream runtimes, so the
// two cannot drift on observe/park/log semantics; internal on purpose —
// consumers receive these signals, only runtimes send them.
//
// Reporter is the entry point: it carries the context every signal needs (which
// runtime, which relay, the observer, the logger) so the runtimes call
// one-argument methods instead of threading four invariant values through every
// dispatch. The free Drained/Error/Sequenced/Swept functions remain for callers
// holding only an Observer.
package notify

import (
	"context"
	"log/slog"
	"time"

	"github.com/quarks-tech/protoevent-go/pkg/transport/outbox"
	"github.com/quarks-tech/protoevent-go/pkg/transport/outbox/internal/leader"
	"github.com/quarks-tech/protoevent-go/pkg/transport/outbox/relay"
)

// Reporter is one relay runtime's signal sink: everything the dispatchers need
// that does not change over the relay's life.
//
// It is a struct rather than a set of closures on purpose — a closure would
// capture the whole enclosing Relay for the process's lifetime, and this copies
// only the four fields the signals actually use.
//
// Not safe for concurrent use: each runtime drives it from its single Run
// goroutine (the leadership latch below is plain state).
type Reporter struct {
	// Runtime names the runtime in log messages: "sequence" or "stream".
	Runtime string
	// Name is the consumer group / relay name every signal is tagged with.
	Name     string
	Observer relay.Observer
	Logger   *slog.Logger

	// wasLeader is the last leadership state reported, so Leadership can fire on
	// transitions only.
	wasLeader bool
}

// Error reports a pass-level or per-message error to the observer.
func (r *Reporter) Error(err error) { Error(r.Observer, r.Name, err) }

// Drained reports a completed drain/forward pass.
func (r *Reporter) Drained(count int, oldestAge time.Duration, more bool) {
	Drained(r.Observer, r.Name, count, oldestAge, more)
}

// Sequenced reports a sequencer pass (sequence runtime only).
func (r *Reporter) Sequenced(count int) { Sequenced(r.Observer, r.Name, count) }

// Swept reports a retention sweep pass (sequence runtime only).
func (r *Reporter) Swept(count int) { Swept(r.Observer, r.Name, count) }

// PassFailed reports a failed relay pass to both the observer and the log. Call
// it only when the run context is still alive: a pass that failed because of a
// planned shutdown is not an incident, and reporting it trains operators to
// ignore the signal.
func (r *Reporter) PassFailed(err error) {
	r.Error(err)
	r.Logger.Error(r.Runtime+" relay: pass failed", "relay", r.Name, "err", err)
}

// Leadership fires the OnLeadership signal and an Info log on TRANSITIONS only.
//
// Without it a standby takeover, or a stale leader resuming after a wedge,
// leaves no trace in either instance's telemetry and the handover timeline
// cannot be reconstructed. Steady-state renewals are not transitions and are
// not reported.
func (r *Reporter) Leadership(isLeader bool) {
	if isLeader == r.wasLeader {
		return
	}
	r.wasLeader = isLeader
	Leadership(r.Observer, r.Logger, r.Runtime, r.Name, isLeader)
}

// ReleaseLeadership performs a relay's whole shutdown leadership step: drop the
// lock so a standby takes over in well under one lease TTL, then report the
// transition so telemetry does not leave a dead instance marked leader.
//
// The release error is informational, not actionable — failover falls back to
// TTL expiry, and a store outage also surfaces on the successor's TryAcquire —
// so it is logged at Warn here rather than returned to a shutdown path that
// could do nothing else with it.
func (r *Reporter) ReleaseLeadership(e *leader.Elector) {
	if err := e.Release(); err != nil {
		r.Logger.Warn(r.Runtime+" relay: release leader lock", "relay", r.Name, "err", err)
	}
	r.Leadership(false)
}

// MessageFailure routes one failed message through the shared failure policy:
// the PoisonHandler (nil for stop-the-lane failures — only poison DecodeErrors
// are ever parked), the Observer, and the Logger. Returns the handler's error
// verbatim (nil when no handler is configured): a non-nil return means the
// park was NOT confirmed and the caller must not advance past the message.
//
// The log field is outbox_id, not event_id: msg.ID is the outbox ROW key,
// which equals the CloudEvents event ID only under the default
// ReuseMetadataID generator — under GenerateUUIDv4 the two differ, and a
// field named event_id would correlate the wrong events.
func (r *Reporter) MessageFailure(ctx context.Context, h relay.PoisonHandler, msg *outbox.Message, err error) error {
	var parkErr error
	if h != nil {
		parkErr = h(ctx, msg, err)
	}
	r.Error(err)
	r.Logger.Error(r.Runtime+" relay: message failed", "relay", r.Name, "outbox_id", msg.ID, "err", err)
	if parkErr != nil {
		// The park failure is its own operational event — a broken DLQ/parking
		// store, distinct from the message fault that triggered it. Without
		// its own signal, alerting sees only the poison error while the lane
		// silently stops retrying the park against a dead DLQ.
		r.Error(parkErr)
		r.Logger.Error(r.Runtime+" relay: poison park failed", "relay", r.Name, "outbox_id", msg.ID, "err", parkErr)
	}

	return parkErr
}

// StuckLane reports a wedged lane — one stopped on the SAME message past the
// escalation threshold — to the observer and the log. Called only when
// StuckTracker.Stuck returns escalate, which is why building position and id
// labels is not on the per-message path.
func (r *Reporter) StuckLane(position, id string, stuckFor time.Duration, err error) {
	r.Logger.Error(r.Runtime+" relay: log wedged on one message — no event behind it is being delivered",
		"relay", r.Name, "position", position, "outbox_id", id, "stuck_for", stuckFor, "err", err,
		"remedy", "if the downstream will never accept this message, configure WithUnsendableClassifier to park it; otherwise fix the downstream")
	r.Error(&relay.StuckLaneError{
		Position: position,
		ID:       id,
		StuckFor: stuckFor,
		Err:      err,
	})
}

// Drained, Error, Sequenced, and Swept are the nil-safe dispatchers for
// relay.Observer's callbacks. They live here rather than as methods on the
// public type so the exported surface stays a pure callback struct — one name
// per signal (httptrace style) — while the runtimes keep one-line call sites.

// Drained invokes o.OnDrained if set.
func Drained(o relay.Observer, name string, count int, oldestAge time.Duration, more bool) {
	if o.OnDrained != nil {
		o.OnDrained(name, count, oldestAge, more)
	}
}

// Error invokes o.OnError if set.
func Error(o relay.Observer, name string, err error) {
	if o.OnError != nil {
		o.OnError(name, err)
	}
}

// Sequenced invokes o.OnSequenced if set.
func Sequenced(o relay.Observer, name string, count int) {
	if o.OnSequenced != nil {
		o.OnSequenced(name, count)
	}
}

// Swept invokes o.OnSwept if set.
func Swept(o relay.Observer, name string, count int) {
	if o.OnSwept != nil {
		o.OnSwept(name, count)
	}
}

// Leadership invokes o.OnLeadership if set AND logs the transition at Info —
// leadership changes are rare, operationally significant, and the log line is
// the zero-configuration trace (the callback is for metrics).
func Leadership(o relay.Observer, log *slog.Logger, runtime, name string, isLeader bool) {
	if o.OnLeadership != nil {
		o.OnLeadership(name, isLeader)
	}
	msg := runtime + " relay: lost leadership"
	if isLeader {
		msg = runtime + " relay: became leader"
	}
	log.Info(msg, "relay", name)
}
