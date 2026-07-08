package relay

import (
	"context"
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
	isLeader bool
}

// NewLeaderElector builds a LeaderElector over store. If store does not
// implement LeaderStore, the returned elector always reports leadership
// (single-instance deployments need no lock).
func NewLeaderElector(store any, lockName, holderID string, ttl time.Duration) *LeaderElector {
	ls, _ := store.(LeaderStore)
	return &LeaderElector{ls: ls, lockName: lockName, holderID: holderID, ttl: ttl}
}

// TryAcquire acquires or renews the lock. Returns true if this elector holds
// it after the call.
func (e *LeaderElector) TryAcquire(ctx context.Context) (bool, error) {
	if e.ls == nil {
		e.isLeader = true
		return true, nil
	}
	held, err := e.ls.TryAcquireLeaderLock(ctx, e.lockName, e.holderID, e.ttl)
	if err != nil {
		return false, err
	}
	e.isLeader = held
	return held, nil
}

// Release drops the lock if this elector currently holds it. No-op if there
// is no LeaderStore or leadership was never acquired. Uses a fresh
// context.Background() with an internal timeout, not the caller's ctx, since
// this typically runs on a shutdown path where ctx may already be canceled.
func (e *LeaderElector) Release() {
	if e.ls == nil || !e.isLeader {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), releaseTimeout)
	defer cancel()
	_ = e.ls.ReleaseLeaderLock(ctx, e.lockName, e.holderID)
	e.isLeader = false
}
