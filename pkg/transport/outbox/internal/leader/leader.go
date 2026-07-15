package leader

import (
	"context"
	"sync/atomic"
	"time"

	"github.com/quarks-tech/protoevent-go/pkg/transport/outbox/relay"
)

// releaseTimeout bounds ReleaseLeaderLock on a fresh context.Background(),
// since Release is typically called from a deferred shutdown path where the
// caller's ctx may already be canceled.
const releaseTimeout = 5 * time.Second

// Elector wraps a store's optional LeaderStore capability with
// acquire/renew and graceful-release. A store that does not implement
// LeaderStore is treated as always-leader (single-instance deployments).
//
// TryAcquire and Release keep store I/O outside the internal lock, so they
// must not be invoked concurrently with each other: a Release racing a
// TryAcquire can delete the lock row the concurrent TryAcquire just won,
// leaving a caller that believes it is leader without a lock. The relay
// runtimes call both from the single Run goroutine; other callers must
// provide the same serialization.
type Elector struct {
	ls       relay.LeaderStore
	lockName string
	holderID string
	ttl      time.Duration

	// isLeader is atomic for visibility only (the deferred Release path may
	// run on a different goroutine than the last TryAcquire): check-then-act
	// atomicity is deliberately NOT provided here — see the serialization
	// contract above.
	isLeader atomic.Bool
}

// nopLeaderStore is the null LeaderStore: every acquire is granted and
// release is a no-op. NewElector substitutes it for a nil ls — it is
// the EXPLICIT single-instance mode, not an implementation of the LeaderStore
// contract: it provides no mutual exclusion and consults no clock. Do not use
// it as a reference implementation.
type nopLeaderStore struct{}

func (nopLeaderStore) TryAcquireLeaderLock(context.Context, string, string, time.Duration) (bool, error) {
	return true, nil
}

func (nopLeaderStore) ReleaseLeaderLock(context.Context, string, string) error { return nil }

// NewElector builds a Elector over ls. ls may be nil: a nil
// LeaderStore means no election — a nopLeaderStore is substituted and the
// elector always reports leadership (single-instance deployments).
func NewElector(ls relay.LeaderStore, lockName, holderID string, ttl time.Duration) *Elector {
	if ls == nil {
		ls = nopLeaderStore{}
	}
	return &Elector{ls: ls, lockName: lockName, holderID: holderID, ttl: ttl}
}

// TryAcquire acquires or renews the lock. Returns true if this elector holds
// it after the call.
//
// The store call is bounded by the lease TTL: an acquire that outlasts the
// lease it would grant is moot, and an unbounded call on a wedged connection
// would silently stall the caller's single relay goroutine past its own
// lease — the silent-stale-leader hazard both runtimes guard every store
// operation against.
func (e *Elector) TryAcquire(ctx context.Context) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, e.ttl)
	defer cancel()
	held, err := e.ls.TryAcquireLeaderLock(ctx, e.lockName, e.holderID, e.ttl)
	if err != nil {
		return false, err
	}
	e.isLeader.Store(held)
	return held, nil
}

// Release drops the lock if this elector currently holds it. No-op (nil) if
// leadership was never acquired. Uses a fresh context.Background() with an
// internal timeout, not the caller's ctx, since this typically runs on a
// shutdown path where ctx may already be canceled.
//
// The flag is swapped false BEFORE the store call: a lost graceful release
// costs at most one lease-TTL of delayed failover (the lock expires on its
// own), while flipping after would let a second Release during a slow store
// call issue a duplicate delete.
//
// The returned error is informational, not actionable: failover falls back to
// TTL expiry regardless, and a store outage also surfaces on the successor's
// TryAcquire every tick. Callers on a shutdown path should log it and move on
// (the relay runtimes do), not fail shutdown over it.
func (e *Elector) Release() error {
	if !e.isLeader.Swap(false) {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), releaseTimeout)
	defer cancel()
	return e.ls.ReleaseLeaderLock(ctx, e.lockName, e.holderID)
}
