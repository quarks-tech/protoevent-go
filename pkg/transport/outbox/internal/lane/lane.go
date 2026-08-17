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
	"time"

	"github.com/quarks-tech/protoevent-go/pkg/event"
	"github.com/quarks-tech/protoevent-go/pkg/eventbus"
	"github.com/quarks-tech/protoevent-go/pkg/transport/outbox"
	"github.com/quarks-tech/protoevent-go/pkg/transport/outbox/internal/bound"
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
	// SendTimeout bounds one Sender.Send, the way OpTimeout bounds one store call
	// (see internal/bound). Both runtimes pass their OpTimeout: a send and a store
	// call are the same kind of hazard — a remote peer that holds the connection
	// open and never answers — and the relay has one budget for that.
	//
	// Non-positive leaves the send unbounded, which is the pre-bound behavior and
	// exists only so a Lane built as a bare struct literal stays usable. Both
	// runtimes set it.
	SendTimeout time.Duration

	stuck notify.StuckTracker[P]
}

// Send delivers msg and applies the failure policy, returning what the caller
// must do with pos.
//
// The branches, in the order they are decided:
//
//   - A canceled run context is a shutdown, not a message fault: stop without
//     parking a healthy message and without reporting an incident.
//   - A failure that is PERMANENT for this message — claimed by the Unsendable
//     classifier, or carrying the transports' own event.ErrUnsendable marker (see
//     permanent) — is parked like a poison row. Retrying it could never succeed, and leaving it
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
	sendErr := l.send(ctx, msg)
	if sendErr == nil {
		l.Progress(pos)

		return Sent
	}
	if ctx.Err() != nil {
		return Canceled
	}

	if l.permanent(sendErr) {
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

// send delivers msg under the send budget.
//
// A send is bounded for the same reason a store call is: a broker that accepts the
// TCP connection and then never answers — RabbitMQ blocking a publishing
// connection on vm_memory_high_watermark is the everyday case — otherwise stalls
// the relay's single Run goroutine indefinitely. That stall is the worst failure
// in the system AND the quietest: no Drained, no Error, no log, and lane.Stuck is
// only reachable once Send has returned, so the wedged-lane escalation cannot
// fire while it is happening.
//
// A deadline of its own, rather than relying on the caller's context: the run
// context has no deadline, and the transports do not impose one (amqp091's
// confirm wait is a bare select on the confirm channel).
func (l *Lane[P]) send(ctx context.Context, msg *outbox.Message) error {
	if l.SendTimeout <= 0 {
		return l.Sender.Send(ctx, msg.Metadata, msg.Data)
	}

	sendCtx, cancel := bound.Call(ctx, l.SendTimeout)
	defer cancel()

	return l.Sender.Send(sendCtx, msg.Metadata, msg.Data)
}

// permanent reports whether a send failure is permanent for THIS message, and so
// may be parked rather than retried forever.
//
// Two sources, and the built-in one is why this is a method rather than a bare
// call to Unsendable. event.ErrUnsendable is the transports' own marker for
// metadata they can never serialize — a dot-less event type, a reserved
// extension name, a value the wire format cannot carry. Publish-time validation
// keeps such metadata out of the store, but an outbox row is DURABLE: rows
// written before those checks existed still arrive here, and without the marker
// their rejection is an opaque error no classifier claims, so the lane stops on
// that row every tick forever with nothing behind it delivered.
//
// It is honored only when a PoisonHandler exists, because "permanent" authorizes
// advancing past the message and there would be nowhere to put it — parking with
// a nil handler silently drops the event (Reporter.MessageFailure returns nil
// when no handler is configured). With no handler the lane stops instead, which
// is exactly how it already treats an undecodable poison row.
func (l *Lane[P]) permanent(err error) bool {
	if l.Poison != nil && errors.Is(err, event.ErrUnsendable) {
		return true
	}

	return l.Unsendable != nil && l.Unsendable(err)
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
