package stream

import (
	"context"
	"time"
)

// boundedStore decorates the relay's Store with per-operation bounds: without
// a deadline, a Mongo call that wedges at the network layer would block the
// single Run goroutine forever — the lease silently expires, a standby takes
// over, and this instance neither errors nor logs (a silent permanent stall),
// then resumes as a stale leader when the call finally unblocks. Beyond
// LeaseTTL the call's success is moot anyway: the lease is already lost.
//
// The bound is applied by decoration at construction (NewRelay), not by
// per-call helpers in the run loop: the policy lives in one type, and a Store
// method added later is bounded by construction instead of by remembering a
// wrapper. LeaderElector.TryAcquire self-bounds the same way. Stream.Next is
// deliberately NOT bounded here — its wait is bounded server-side by
// maxAwaitTime (the DrainWindow passed to Watch).
type boundedStore struct {
	inner Store
	ttl   time.Duration
}

func (s boundedStore) LoadToken(ctx context.Context, name string) (string, time.Time, error) {
	ctx, cancel := context.WithTimeout(ctx, s.ttl)
	defer cancel()
	return s.inner.LoadToken(ctx, name)
}

// SaveToken is additionally shutdown-safe: once the run ctx is canceled (the
// final save on a planned shutdown), the store call gets a values-preserving
// detached context (WithoutCancel keeps trace/log values) bounded by
// shutdownTimeout instead — a real store fails writes on a dead context, and
// losing that save would redeliver up to TokenBatchSize-1 acknowledged sends
// per deploy. The dead-ctx check MUST come first; the consumer test suite
// pins it (saveHadDeadline/saveCtxErr). Mirrors sequence/bounded.go
// CommitOffset — keep the dead-ctx detachment in lockstep.
func (s boundedStore) SaveToken(ctx context.Context, name string, token string, commitTime time.Time) (bool, error) {
	// ALWAYS detached from the run ctx's cancellation, not just when it is
	// already dead: the save records sends that already happened, so a
	// cancel racing in AFTER the ctx.Err() check but DURING the write would
	// otherwise abort it and redeliver up to TokenBatchSize-1 acknowledged
	// sends on restart. Values (trace/log) are preserved; the bound alone
	// limits it. Mirrors sequence/bounded.go CommitOffset.
	timeout := s.ttl
	if ctx.Err() != nil {
		timeout = shutdownTimeout
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), timeout)
	defer cancel()
	return s.inner.SaveToken(ctx, name, token, commitTime)
}

// Watch's bound covers only opening the stream (the initial aggregate); the
// returned cursor is not tied to the bounded context.
func (s boundedStore) Watch(ctx context.Context, token string, maxAwait time.Duration) (Stream, error) {
	ctx, cancel := context.WithTimeout(ctx, s.ttl)
	defer cancel()
	return s.inner.Watch(ctx, token, maxAwait)
}
