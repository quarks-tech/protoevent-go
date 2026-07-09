package relay

import (
	"context"
	"sync"
	"time"
)

// releaseTimeout bounds ReleaseLeaderLock on a fresh context.Background(),
// since Release is typically called from a deferred shutdown path where the
// caller's ctx may already be canceled.
const releaseTimeout = 5 * time.Second

// LeaderElector wraps a store's optional LeaderStore capability with
// acquire/renew and graceful-release. A store that does not implement
// LeaderStore is treated as always-leader (single-instance deployments).
type LeaderElector struct {
	ls       LeaderStore
	lockName string
	holderID string
	ttl      time.Duration

	mu       sync.Mutex // guards isLeader: TryAcquire/Release may be called concurrently
	isLeader bool
}

// NewLeaderElector builds a LeaderElector over ls. ls may be nil: a nil
// LeaderStore means no election — the elector always reports leadership
// (single-instance deployments).
func NewLeaderElector(ls LeaderStore, lockName, holderID string, ttl time.Duration) *LeaderElector {
	return &LeaderElector{ls: ls, lockName: lockName, holderID: holderID, ttl: ttl}
}

// TryAcquire acquires or renews the lock. Returns true if this elector holds
// it after the call.
func (e *LeaderElector) TryAcquire(ctx context.Context) (bool, error) {
	if e.ls == nil {
		e.mu.Lock()
		e.isLeader = true
		e.mu.Unlock()
		return true, nil
	}
	held, err := e.ls.TryAcquireLeaderLock(ctx, e.lockName, e.holderID, e.ttl)
	if err != nil {
		return false, err
	}
	e.mu.Lock()
	e.isLeader = held
	e.mu.Unlock()
	return held, nil
}

// Release drops the lock if this elector currently holds it. No-op if there
// is no LeaderStore or leadership was never acquired. Uses a fresh
// context.Background() with an internal timeout, not the caller's ctx, since
// this typically runs on a shutdown path where ctx may already be canceled.
func (e *LeaderElector) Release() {
	e.mu.Lock()
	if e.ls == nil || !e.isLeader {
		e.mu.Unlock()
		return
	}
	e.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), releaseTimeout)
	defer cancel()
	_ = e.ls.ReleaseLeaderLock(ctx, e.lockName, e.holderID)

	e.mu.Lock()
	e.isLeader = false
	e.mu.Unlock()
}
