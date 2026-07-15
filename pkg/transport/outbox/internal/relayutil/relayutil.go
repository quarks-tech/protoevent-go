// Package relayutil holds implementation seams shared by the relay/sequence
// and relay/stream runtimes. It is internal on purpose: these helpers exist
// so the two runtimes cannot drift on shutdown-context and message-failure
// semantics — they are not part of the outbox module's public contract.
package relayutil

import (
	"context"
	"log/slog"
	"time"

	"github.com/quarks-tech/protoevent-go/pkg/transport/outbox"
	"github.com/quarks-tech/protoevent-go/pkg/transport/outbox/relay"
)

// BoundContext returns the context for one relay store operation. While ctx is
// alive it is bounded by liveTimeout (0 = no bound, ctx returned as-is); once
// ctx is canceled — the final offset commit / token save / stream close on a
// planned shutdown — it is replaced by a values-preserving detached context
// (context.WithoutCancel, so trace spans and log attrs survive into shutdown
// writes) bounded by deadTimeout, keeping those writes deliverable and bounded
// (a real store fails writes on a dead context, and losing the final commit
// redelivers acknowledged sends on restart). Always call the returned cancel.
func BoundContext(ctx context.Context, liveTimeout, deadTimeout time.Duration) (context.Context, context.CancelFunc) {
	if ctx.Err() != nil {
		return context.WithTimeout(context.WithoutCancel(ctx), deadTimeout)
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
func HandleMessageError(ctx context.Context, h relay.ErrorHandler, obs relay.Observer, log *slog.Logger, runtime, name string, msg *outbox.Message, err error) {
	if h != nil {
		h(ctx, msg, err)
	}
	ObserveError(obs, name, err)
	log.Error(runtime+" relay: message failed", "relay", name, "event_id", msg.ID, "err", err)
}

// ObserveDrained, ObserveError, and ObserveSequenced are the nil-safe
// dispatchers for relay.Observer's callbacks. They live here rather than as
// methods on the public type so the exported surface stays a pure callback
// struct — one name per signal (httptrace style) — while the runtimes keep
// one-line call sites.

// ObserveDrained invokes o.OnDrained if set.
func ObserveDrained(o relay.Observer, name string, count int, oldestAge time.Duration, more bool) {
	if o.OnDrained != nil {
		o.OnDrained(name, count, oldestAge, more)
	}
}

// ObserveError invokes o.OnError if set.
func ObserveError(o relay.Observer, name string, err error) {
	if o.OnError != nil {
		o.OnError(name, err)
	}
}

// ObserveSequenced invokes o.OnSequenced if set.
func ObserveSequenced(o relay.Observer, name string, count int) {
	if o.OnSequenced != nil {
		o.OnSequenced(name, count)
	}
}
