package notify

import (
	"log/slog"
	"time"

	"github.com/quarks-tech/protoevent-go/pkg/transport/outbox/relay"
)

// StuckEscalateAfter is how long a lane must be stopped at ONE position before it
// escalates from "a send failed" to "this log is wedged". Chosen well above any
// plausible downstream outage: a threshold that fired during a routine blip would
// train operators to ignore the one signal that means manual intervention.
const StuckEscalateAfter = 15 * time.Minute

// StuckTracker escalates a lane that keeps stopping at the same position.
//
// Both relay runtimes stop their lane on a failure and retry the same message next
// pass, which is right for an outage and indistinguishable — tick by tick — from a
// message the downstream will never accept. This turns "the same position failed
// again" into a single escalation per episode, so an alert can act on a genuine
// wedge without firing on every transient failure.
//
// P is the runtime's own position type — an int64 seq for the sequenced log, a
// resume-token string for the change stream — and is compared directly. It is
// deliberately NOT a pre-formatted label: Progress runs on the HOT PATH, once per
// successfully delivered message, and formatting a label there costs an allocation
// per message to build a string that a mismatched compare immediately discards
// (measured at ~0.9 allocs/message and a third of drain wall time before this was
// made generic). Labels are built only when an escalation actually fires.
//
// Not safe for concurrent use: each runtime drives it from its single Run goroutine.
type StuckTracker[P comparable] struct {
	position P
	// active distinguishes "stopped at the zero position" from "not stopped at
	// all" — seq 0 is a legitimate position, so the zero P cannot mean absence.
	active    bool
	since     time.Time
	escalated bool
}

// Progress records a successful disposal (a send, or a confirmed park) at position,
// ending the episode only if THAT is the position the lane was stuck at.
//
// The position check is the whole point. Clearing on any successful disposal looks
// equivalent and is not: a pass that re-delivers a prefix AHEAD of the wedged
// message — which is what happens whenever the offset or token save is failing or is
// being rejected by a monotone guard — would then reset the timer on every pass, and
// a permanently wedged lane would never reach the threshold. Those re-deliveries are
// at earlier positions, so they no longer match.
func (s *StuckTracker[P]) Progress(pos P) {
	if !s.active || pos != s.position {
		return
	}

	var zero P
	s.position, s.active, s.since, s.escalated = zero, false, time.Time{}, false
}

// Stuck records that the lane stopped at pos and reports how long it has been
// stopped there, with escalate true at most ONCE per episode — the caller reports
// only when it is true, so the (allocating) label formatting stays off the hot path.
//
// Must be called from EVERY path that stops the lane: a send failure, a
// classified-unsendable message whose park was not confirmed, and a poison row that
// was not parked (whether because no handler is configured — the default — or
// because the park failed). A path that stops without reporting here leaves the
// wedge with no signal beyond the generic per-tick error, which is the gap this
// type exists to close.
func (s *StuckTracker[P]) Stuck(pos P) (stuckFor time.Duration, escalate bool) {
	if !s.active || pos != s.position {
		s.position, s.active, s.since, s.escalated = pos, true, time.Now(), false

		return 0, false
	}
	if s.escalated || s.since.IsZero() {
		return 0, false
	}

	stuckFor = time.Since(s.since)
	if stuckFor < StuckEscalateAfter {
		return 0, false
	}
	s.escalated = true

	return stuckFor, true
}

// StuckLane reports a wedged lane to the observer and the log. Called only when
// StuckTracker.Stuck returns escalate — position and id are formatted by the caller
// at that point, which is why building them is not on the per-message path.
func StuckLane(
	o relay.Observer,
	log *slog.Logger,
	runtime, name, position, id string,
	stuckFor time.Duration,
	err error,
) {
	log.Error(runtime+" relay: log wedged on one message — no event behind it is being delivered",
		"relay", name, "position", position, "outbox_id", id, "stuck_for", stuckFor, "err", err,
		"remedy", "if the downstream will never accept this message, configure WithUnsendableClassifier to park it; otherwise fix the downstream")
	Error(o, name, &relay.StuckLaneError{
		Position: position,
		ID:       id,
		StuckFor: stuckFor,
		Err:      err,
	})
}
