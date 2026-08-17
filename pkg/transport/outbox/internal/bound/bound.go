// Package bound holds the relay runtimes' operation-timeout policy: how long a
// store call may take, and which store calls must survive a canceled run context.
//
// It exists because those rules are safety-critical and used to be written out at
// every call site in both runtimes. Three policies, deliberately distinct:
//
//   - Call: the default. Bounded by the relay's OpTimeout, cancels with the run
//     context. (The leader lock is the exception: it self-bounds by LeaseTTL,
//     since a lock call outliving the lease it is acquiring is pointless.)
//   - Commit: for a write that records work the relay has ALREADY done (an
//     offset commit, a resume-token save). Always detached from cancellation.
//   - Salvaged: for a best-effort READ that legitimately runs after cancellation
//     (a lag clock read on the shutdown path). Detached only once the context is
//     already dead, so a cancel arriving mid-call still aborts it.
//
// Why any bound at all: neither database/sql nor the mongo driver has a default
// operation timeout, so an unbounded call on a wedged connection stalls the single
// Run goroutine with no error and no log while the lease quietly expires and a
// standby takes over — see boundedStream in relay/stream for the full mechanism.
package bound

import (
	"context"
	"time"
)

// Shutdown is the budget for store I/O on a shutdown path, where the run
// context is already dead and the lease TTL is no longer the relevant bound.
// One constant for every such path: the final offset commit, the final token
// save, the change-stream close, and the leader-lock release.
const Shutdown = 5 * time.Second

// Call bounds an ordinary store call to ttl, canceling with ctx.
func Call(ctx context.Context, ttl time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, ttl)
}

// Commit bounds a store write that records work the relay has already
// performed, and detaches it from the run context's cancellation.
//
// ALWAYS detached, not just when ctx is already dead: the write records sends
// that already happened, so a cancel racing in after a ctx.Err() check but
// DURING the write would abort it and redeliver the acknowledged work on
// restart — a whole drained page for the sequence runtime, up to
// TokenBatchSize-1 events for the stream runtime. Values (trace/log) are
// preserved via WithoutCancel; the timeout alone limits the call.
//
// The budget drops to Shutdown once ctx is dead, because at that point the
// lease TTL is not the constraint — the process is leaving.
func Commit(ctx context.Context, ttl time.Duration) (context.Context, context.CancelFunc) {
	timeout := ttl
	if ctx.Err() != nil {
		timeout = Shutdown
	}

	return context.WithTimeout(context.WithoutCancel(ctx), timeout)
}

// Salvaged bounds a best-effort read that may legitimately be issued after the
// run context is canceled, detaching it only in that case.
//
// The asymmetry with Commit is deliberate. A drain pass reports its telemetry —
// and so reads the store clock for the lag value — after it has already observed
// cancellation, and on a dead context a real store fails the query immediately,
// so every planned shutdown would otherwise degrade lag to the host-clock
// fallback and warn about a clock that is fine. But unlike a commit, this call
// records nothing: a cancel arriving mid-call SHOULD abort it rather than hold
// shutdown open for the full budget.
func Salvaged(ctx context.Context, ttl time.Duration) (context.Context, context.CancelFunc) {
	if ctx.Err() != nil {
		return context.WithTimeout(context.WithoutCancel(ctx), Shutdown)
	}

	return context.WithTimeout(ctx, ttl)
}

// Fresh bounds shutdown-path I/O that has no usable parent context at all — a
// deferred leader-lock release, a change-stream close — where the caller's
// context is typically already canceled and the call would otherwise never
// reach the server.
func Fresh() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), Shutdown)
}
