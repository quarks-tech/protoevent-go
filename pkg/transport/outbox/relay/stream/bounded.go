package stream

import (
	"context"
	"time"

	"github.com/quarks-tech/protoevent-go/pkg/transport/outbox/internal/bound"
)

// boundedStore decorates the relay's Store with the shared operation-timeout
// policy (internal/bound): every call carries the OpTimeout bound, and the token
// save — which records already-completed work — is additionally detached from
// the run context's cancellation.
//
// The bound is applied by decoration at construction (NewRelay), not by per-call
// helpers in the run loop: the policy lives in one type, and a Store method
// added later is bounded by construction instead of by remembering a wrapper.
// leader.Elector.TryAcquire self-bounds the same way. The Stream that Watch
// returns is decorated too (boundedStream) — see there.
type boundedStore struct {
	inner Store
	ttl   time.Duration
}

func (s boundedStore) LoadToken(ctx context.Context, name string) (string, time.Time, error) {
	ctx, cancel := bound.Call(ctx, s.ttl)
	defer cancel()

	return s.inner.LoadToken(ctx, name)
}

// SaveToken uses bound.Commit: the save records sends that already happened, so
// losing it to a canceled run context would redeliver up to TokenBatchSize-1
// acknowledged sends per deploy. The consumer test suite pins the detachment
// (saveHadDeadline/saveCtxErr).
func (s boundedStore) SaveToken(ctx context.Context, name string, token string, commitTime time.Time) (bool, error) {
	ctx, cancel := bound.Commit(ctx, s.ttl)
	defer cancel()

	return s.inner.SaveToken(ctx, name, token, commitTime)
}

// Watch's bound covers only opening the stream (the initial aggregate); the
// returned cursor is not tied to the bounded context. The cursor itself is
// decorated so its own calls are bounded — see boundedStream.
func (s boundedStore) Watch(ctx context.Context, token string, maxAwait time.Duration) (Stream, error) {
	openCtx, cancel := bound.Call(ctx, s.ttl)
	defer cancel()

	inner, err := s.inner.Watch(openCtx, token, maxAwait)
	if err != nil {
		return nil, err
	}

	return boundedStream{inner: inner, ttl: s.ttl}, nil
}

// boundedStream carries boundedStore's policy onto the cursor.
//
// Next is the relay's hot path and the one call that had no client-side bound at
// all. maxAwaitTime looks like the bound and is not: it is a SERVER-side limit on
// how long the server waits before replying, so it cannot fire when the reply
// never arrives — a blackholed TCP connection (a dropped NAT or conntrack entry,
// no RST) leaves the read blocking for the OS timeout or indefinitely. The single
// Run goroutine then stalls with none of the symptoms anyone watches for: no
// error, no log line, no OnDrained, while the lease quietly expires, a standby
// takes over the same log, and this instance eventually wakes as a stale leader
// and re-sends what the standby already delivered. That is precisely the silent
// permanent stall the decorator exists to prevent.
//
// The bound is OpTimeout, as everywhere else here — deliberately NOT LeaseTTL,
// which it used to be. An idle Next blocks server-side for a full maxAwait
// (= DrainWindow) by design, so the client-side bound has to sit above it or it
// fires on healthy idle windows; NewRelay requires DrainWindow < OpTimeout for
// exactly that. Tying it to the lease instead made every shortening of the
// failover budget also shorten this one, which is unrelated to leadership: a
// wedged connection is a wedged connection whether or not the lease is still
// held. A timeout surfaces as an ordinary transient error, which Run handles by
// closing and reopening the stream.
//
// The bound is per CALL, not per drain window, even though a per-window deadline
// would spend one timer instead of TokenBatchSize of them: a window also contains
// the sends between the Next calls, and a window is explicitly allowed to outlive
// the lease when the Sender is slow (see NewRelay's sizing note). A window-wide
// deadline would convert that documented case into a spurious cursor reopen.
type boundedStream struct {
	inner Stream
	ttl   time.Duration
}

func (s boundedStream) Next(ctx context.Context) (*Event, bool, error) {
	ctx, cancel := bound.Call(ctx, s.ttl)
	defer cancel()

	return s.inner.Next(ctx)
}

func (s boundedStream) Checkpoint() (string, time.Time, bool) {
	return s.inner.Checkpoint()
}

// Close is left with the caller's context: closeStream already builds a fresh
// bounded one (the run ctx is dead on the shutdown paths Close runs on), and
// wrapping that in a second, longer timeout would only obscure it.
func (s boundedStream) Close(ctx context.Context) error {
	return s.inner.Close(ctx)
}
