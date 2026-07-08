package relay_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/quarks-tech/protoevent-go/pkg/transport/outbox/relay"
)

// releaseCall records one ReleaseLeaderLock invocation, including whether the
// ctx passed to it was already done (used to sanity-check Release's own
// internal context).
type releaseCall struct {
	name, holderID string
	ctxErr         error
}

// fakeLeaderStore is a configurable relay.LeaderStore fake: TryAcquire can be
// scripted to grant, deny, or error; Release can be scripted to succeed,
// error, or just record its calls.
type fakeLeaderStore struct {
	mu sync.Mutex

	acquireGrant bool
	acquireErr   error
	acquireCalls int

	releaseErr   error
	releaseCalls []releaseCall
}

func (s *fakeLeaderStore) TryAcquireLeaderLock(_ context.Context, _, _ string, _ time.Duration) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.acquireCalls++
	if s.acquireErr != nil {
		return false, s.acquireErr
	}
	return s.acquireGrant, nil
}

func (s *fakeLeaderStore) ReleaseLeaderLock(ctx context.Context, name, holderID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.releaseCalls = append(s.releaseCalls, releaseCall{name: name, holderID: holderID, ctxErr: ctx.Err()})
	return s.releaseErr
}

func (s *fakeLeaderStore) snapshotReleaseCalls() []releaseCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]releaseCall, len(s.releaseCalls))
	copy(out, s.releaseCalls)
	return out
}

func TestTryAcquireGrantsLeadership(t *testing.T) {
	store := &fakeLeaderStore{acquireGrant: true}
	e := relay.NewLeaderElector(store, "lock", "holder-1", time.Second)

	leader, err := e.TryAcquire(t.Context())
	if err != nil {
		t.Fatalf("TryAcquire: %v", err)
	}
	if !leader {
		t.Fatal("TryAcquire = false, want true (store granted)")
	}
}

func TestTryAcquireRenewsWhileAlreadyLeader(t *testing.T) {
	store := &fakeLeaderStore{acquireGrant: true}
	e := relay.NewLeaderElector(store, "lock", "holder-1", time.Second)

	for i := range 3 {
		leader, err := e.TryAcquire(t.Context())
		if err != nil {
			t.Fatalf("TryAcquire[%d]: %v", i, err)
		}
		if !leader {
			t.Fatalf("TryAcquire[%d] = false, want true (renew)", i)
		}
	}
	if store.acquireCalls != 3 {
		t.Fatalf("acquireCalls = %d, want 3 (each renewal must call the store)", store.acquireCalls)
	}
}

func TestTryAcquireDeniedByAnotherHolder(t *testing.T) {
	store := &fakeLeaderStore{acquireGrant: false}
	e := relay.NewLeaderElector(store, "lock", "holder-1", time.Second)

	leader, err := e.TryAcquire(t.Context())
	if err != nil {
		t.Fatalf("TryAcquire: %v", err)
	}
	if leader {
		t.Fatal("TryAcquire = true, want false (store denied)")
	}
}

func TestTryAcquireErrorPropagates(t *testing.T) {
	sentinel := errors.New("store unavailable")
	store := &fakeLeaderStore{acquireErr: sentinel}
	e := relay.NewLeaderElector(store, "lock", "holder-1", time.Second)

	leader, err := e.TryAcquire(t.Context())
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want %v", err, sentinel)
	}
	if leader {
		t.Fatal("TryAcquire = true on store error, want false")
	}
}

func TestReleaseNoOpWhenNotLeader(t *testing.T) {
	store := &fakeLeaderStore{acquireGrant: false}
	e := relay.NewLeaderElector(store, "lock", "holder-1", time.Second)

	if _, err := e.TryAcquire(t.Context()); err != nil {
		t.Fatalf("TryAcquire: %v", err)
	}
	e.Release()

	if calls := store.snapshotReleaseCalls(); len(calls) != 0 {
		t.Fatalf("ReleaseLeaderLock called %d times, want 0 (never acquired leadership)", len(calls))
	}
}

func TestReleaseNoOpBeforeFirstAcquire(t *testing.T) {
	store := &fakeLeaderStore{acquireGrant: true}
	e := relay.NewLeaderElector(store, "lock", "holder-1", time.Second)

	// Release before ever calling TryAcquire: must be a no-op.
	e.Release()

	if calls := store.snapshotReleaseCalls(); len(calls) != 0 {
		t.Fatalf("ReleaseLeaderLock called %d times, want 0 (TryAcquire never called)", len(calls))
	}
}

func TestReleaseCallsStoreForCurrentHolderWhenLeader(t *testing.T) {
	store := &fakeLeaderStore{acquireGrant: true}
	e := relay.NewLeaderElector(store, "my-lock", "holder-42", time.Second)

	if _, err := e.TryAcquire(t.Context()); err != nil {
		t.Fatalf("TryAcquire: %v", err)
	}
	e.Release()

	calls := store.snapshotReleaseCalls()
	if len(calls) != 1 {
		t.Fatalf("ReleaseLeaderLock called %d times, want 1", len(calls))
	}
	if calls[0].name != "my-lock" || calls[0].holderID != "holder-42" {
		t.Fatalf("release call = %+v, want name=my-lock holderID=holder-42", calls[0])
	}
	// Release passes its own context (fresh, with an internal timeout), never
	// one that is already canceled/expired at call time — there is no
	// caller-supplied ctx parameter to Release() at all (see leader.go), so
	// this is the only observable proxy for "uses its own context".
	if calls[0].ctxErr != nil {
		t.Fatalf("ctx passed to ReleaseLeaderLock was already done: %v, want a fresh context", calls[0].ctxErr)
	}
}

func TestReleaseIsIdempotentAfterFirstCall(t *testing.T) {
	store := &fakeLeaderStore{acquireGrant: true}
	e := relay.NewLeaderElector(store, "lock", "holder-1", time.Second)

	if _, err := e.TryAcquire(t.Context()); err != nil {
		t.Fatalf("TryAcquire: %v", err)
	}
	e.Release()
	e.Release() // second call: isLeader is now false, must not call the store again

	if calls := store.snapshotReleaseCalls(); len(calls) != 1 {
		t.Fatalf("ReleaseLeaderLock called %d times, want 1 (second Release is a no-op)", len(calls))
	}
}

func TestReleasePropagatesNoPanicOnStoreError(t *testing.T) {
	store := &fakeLeaderStore{acquireGrant: true, releaseErr: errors.New("release boom")}
	e := relay.NewLeaderElector(store, "lock", "holder-1", time.Second)

	if _, err := e.TryAcquire(t.Context()); err != nil {
		t.Fatalf("TryAcquire: %v", err)
	}
	// Release has no return value: a store error must be swallowed, not panic.
	e.Release()

	if calls := store.snapshotReleaseCalls(); len(calls) != 1 {
		t.Fatalf("ReleaseLeaderLock called %d times, want 1", len(calls))
	}
}

func TestNilLeaderStoreAlwaysLeader(t *testing.T) {
	e := relay.NewLeaderElector(nil, "lock", "holder-1", time.Second)

	leader, err := e.TryAcquire(t.Context())
	if err != nil {
		t.Fatalf("TryAcquire: %v", err)
	}
	if !leader {
		t.Fatal("TryAcquire with nil LeaderStore = false, want true (single-instance always-leader)")
	}
}

func TestNilLeaderStoreReleaseIsNoop(t *testing.T) {
	e := relay.NewLeaderElector(nil, "lock", "holder-1", time.Second)

	if _, err := e.TryAcquire(t.Context()); err != nil {
		t.Fatalf("TryAcquire: %v", err)
	}
	// Must not panic despite isLeader==true: nil store short-circuits first.
	e.Release()
}
