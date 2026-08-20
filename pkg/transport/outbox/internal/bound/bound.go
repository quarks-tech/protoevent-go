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
	"sync"
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
// The budget is ttl while the process is alive and drops to Shutdown once ctx is
// canceled, INCLUDING when the cancel arrives mid-call. Deciding that only at entry
// was the bug: a SIGTERM landing a microsecond after this returned left the write
// detached and uninterruptible for the whole ttl — 30s by default — and with the
// stream close and the lock release still to come, shutdown reached ~40s against a
// Kubernetes terminationGracePeriodSeconds that defaults to 30. The pod was then
// SIGKILLed mid-commit, losing the very write this detachment exists to protect and
// redelivering the page it had already sent.
//
// Detachment is preserved either way: the point is that the write is not aborted the
// instant cancellation arrives, not that it may run forever afterwards.
func Commit(ctx context.Context, ttl time.Duration) (context.Context, context.CancelFunc) {
	if ctx.Err() != nil {
		return context.WithTimeout(context.WithoutCancel(ctx), Shutdown)
	}

	out, cancel := context.WithTimeout(context.WithoutCancel(ctx), ttl)

	// Watchdog: shorten the detached budget to Shutdown when the parent cancels. It
	// exits as soon as the caller releases the context or the deadline passes, so it
	// cannot outlive the call it bounds.
	stop := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
		case <-stop:
			return
		case <-out.Done():
			return
		}

		grace := time.NewTimer(Shutdown)
		defer grace.Stop()
		select {
		case <-grace.C:
			cancel()
		case <-stop:
		case <-out.Done():
		}
	}()

	var once sync.Once

	return out, func() {
		once.Do(func() { close(stop) })
		cancel()
	}
}

// ShutdownScope is the single budget shared by every step of a planned shutdown that
// runs after the run context is dead — the leader-lock release and the change-stream
// close.
//
// One scope rather than a Fresh() per step, because the steps are SEQUENTIAL: two
// independent Shutdown budgets are a 2*Shutdown tail, and each new shutdown step added
// later would silently extend it again. Sharing one deadline makes the tail bounded by
// Shutdown no matter how many steps it grows to, which is what lets the total shutdown
// cost be stated at all.
func ShutdownScope() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), Shutdown)
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

// Fresh bounds a single piece of shutdown-path I/O that has no usable parent context
// at all, for a caller that is not part of a shutdown sequence with its own scope.
//
// Prefer ShutdownScope where several such calls run one after another: N independent
// Fresh calls cost N*Shutdown, which is how the shutdown tail grew past the pod's
// termination grace period.
func Fresh() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), Shutdown)
}
