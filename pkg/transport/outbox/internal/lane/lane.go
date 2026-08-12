// Package lane holds the per-message delivery policy shared by both relay
// runtimes: what happens to one message between "the sender was called" and
// "the relay may advance past it".
//
// That policy IS the at-least-once contract — when a failure stops the lane,
// when a message may be parked instead, and when the position may move — and it
// used to be written out twice, once per runtime, differing only in the type of
// the position each tracks. A change made to one copy and not the other is not a
// compile error; it is a delivery bug. Here it exists once, parameterized by the
// position type.
//
// What is genuinely per-runtime stays in the runtime: how the next batch of work
// arrives, and how a position is committed.
package lane

import (
	"context"
	"errors"

	"github.com/quarks-tech/protoevent-go/pkg/eventbus"
	"github.com/quarks-tech/protoevent-go/pkg/transport/outbox"
	"github.com/quarks-tech/protoevent-go/pkg/transport/outbox/internal/notify"
	"github.com/quarks-tech/protoevent-go/pkg/transport/outbox/relay"
)

// Disposition is what happened to one message, and therefore what the caller
// must do with its position.
type Disposition int

const (
	// Sent: delivered downstream. Advance past it.
	Sent Disposition = iota
	// Parked: not delivered, but durably parked via the PoisonHandler, which
	// confirmed the park. Advance past it — retrying could never succeed.
	Parked
	// Stopped: the lane stops here. The message stays pending and is retried on
	// the next pass, preserving both order and delivery. Already reported.
	Stopped
	// Canceled: the run context died mid-send. A shutdown, not a message fault:
	// stop without parking anything and without reporting an incident.
	Canceled
)

// Lane applies the shared per-message policy for one relay runtime.
//
// P is the runtime's own position type — an int64 seq for the sequenced log, a
// resume-token string for the change stream. It is used only as the stuck-lane
// tracking key and is compared directly, never formatted, because Progress runs
// once per delivered message: formatting a label there costs an allocation per
// message to build a string a mismatched compare immediately discards.
//
// Not safe for concurrent use: each runtime drives it from its single Run
// goroutine.
type Lane[P comparable] struct {
	// Reporter dispatches the observer/log signals this policy emits.
	Reporter *notify.Reporter
	Sender   eventbus.Sender
	// Poison parks a message retrying can never fix. Nil means no parking path:
	// such a message stops the lane instead.
	Poison relay.PoisonHandler
	// Unsendable claims send failures that are permanent for one specific
	// message. Nil means every send failure stops the lane.
	Unsendable relay.UnsendableClassifier
	// Label renders a position for the operator. Called ONLY when an escalation
	// fires (at most once per stuck episode), which is what keeps its allocation
	// off the per-message path.
	Label func(pos P, id string) string
	// Identified reports whether pos actually identifies the message. Nil means
	// every position does. It exists for the change-stream runtime, whose
	// poison-event resume token and id are both best-effort extractions and can
	// both come back empty — see notify.StuckTracker.Stuck.
	Identified func(pos P) bool

	stuck notify.StuckTracker[P]
}

// Send delivers msg and applies the failure policy, returning what the caller
// must do with pos.
//
// The branches, in the order they are decided:
//
//   - A canceled run context is a shutdown, not a message fault: stop without
//     parking a healthy message and without reporting an incident.
//   - A failure the Unsendable classifier calls PERMANENT for this message is
//     parked like a poison row. Retrying it could never succeed, and leaving it
//     at the head of the log would wedge every message behind it indefinitely,
//     recoverable only by hand-editing positions in a live database. Only a
//     CONFIRMED park advances past it; an unconfirmed one stops the lane and
//     retries the park next pass, so a transient DLQ outage cannot silently skip
//     an event forever.
//   - Every other send failure stops the lane, PoisonHandler or not. It is
//     downstream trouble (broker down, timeout), not a message fault, and
//     parking healthy messages during an outage would bulk-divert the whole
//     backlog to the DLQ while permanently advancing past it.
func (l *Lane[P]) Send(ctx context.Context, pos P, msg *outbox.Message) Disposition {
	sendErr := l.Sender.Send(ctx, msg.Metadata, msg.Data)
	if sendErr == nil {
		l.Progress(pos)

		return Sent
	}
	if ctx.Err() != nil {
		return Canceled
	}

	if l.Unsendable != nil && l.Unsendable(sendErr) {
		// MessageFailure has already reported this failure, so the stop path
		// below must not report it a second time — double-counting OnError
		// misrepresents the incident's size.
		parkErr := l.Reporter.MessageFailure(ctx, l.Poison, msg, sendErr)
		if parkErr == nil {
			l.Progress(pos)

			return Parked
		}
		// Unsendable AND unparkable: the lane is wedged here for as long as the
		// DLQ stays broken, so the escalation applies exactly as it does to a
		// plain send failure.
		l.Stuck(pos, msg.ID, errors.Join(sendErr, parkErr))

		return Stopped
	}

	// nil handler: the return is always nil (send failures are never parked).
	_ = l.Reporter.MessageFailure(ctx, nil, msg, sendErr)
	l.Stuck(pos, msg.ID, sendErr)

	return Stopped
}

// Park hands a message the relay can never deliver — one whose persisted payload
// failed to decode — to the PoisonHandler, and reports the failure either way.
//
// Returns nil only once the park is CONFIRMED, which is the caller's
// authorization to advance past pos: that advance is irreversible, and an
// unconfirmed DLQ write would silently skip the message forever. A non-nil
// return means the lane must stop at it and retry the park next pass.
//
// The caller is responsible for escalating an unconfirmed park (via Stuck),
// because the no-handler case reaches the same wedge without ever calling here
// and both must report through one site.
func (l *Lane[P]) Park(ctx context.Context, pos P, msg *outbox.Message, cause error) error {
	if err := l.Reporter.MessageFailure(ctx, l.Poison, msg, cause); err != nil {
		return err
	}
	l.Progress(pos)

	return nil
}

// Progress ends a stuck episode after the message at pos was disposed of — sent,
// or parked with confirmation. Keyed on pos rather than unconditional: see
// notify.StuckTracker.Progress.
func (l *Lane[P]) Progress(pos P) { l.stuck.Progress(pos) }

// Stuck escalates a lane that keeps stopping at the same position into a single
// relay.StuckLaneError per episode. It must be called from EVERY path that stops
// the lane — Send does so itself; a caller that stops for its own reasons (a
// poison row with no parking path, an unconfirmed park) must call it directly.
func (l *Lane[P]) Stuck(pos P, id string, err error) {
	identified := true
	if l.Identified != nil {
		identified = l.Identified(pos)
	}

	stuckFor, escalate := l.stuck.Stuck(pos, identified)
	if !escalate {
		return
	}

	l.Reporter.StuckLane(l.Label(pos, id), id, stuckFor, err)
}
