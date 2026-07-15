// Package notify delivers relay runtime signals to the user-configured hooks
// (Observer callbacks, PoisonHandler, Logger), nil-safely. It is the single
// dispatch path shared by the relay/sequence and relay/stream runtimes, so
// the two cannot drift on observe/park/log semantics; internal on purpose —
// consumers receive these signals, only runtimes send them.
package notify

import (
	"context"
	"log/slog"
	"time"

	"github.com/quarks-tech/protoevent-go/pkg/transport/outbox"
	"github.com/quarks-tech/protoevent-go/pkg/transport/outbox/relay"
)

// MessageFailure routes one failed message through the shared failure policy:
// the PoisonHandler (nil for stop-the-lane failures — only poison DecodeErrors
// are ever parked), the Observer, and the Logger.
func MessageFailure(ctx context.Context, h relay.PoisonHandler, obs relay.Observer, log *slog.Logger, runtime, name string, msg *outbox.Message, err error) {
	if h != nil {
		h(ctx, msg, err)
	}
	Error(obs, name, err)
	log.Error(runtime+" relay: message failed", "relay", name, "event_id", msg.ID, "err", err)
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
	if isLeader {
		log.Info(runtime+" relay: became leader", "relay", name)
	} else {
		log.Info(runtime+" relay: lost leadership", "relay", name)
	}
}
