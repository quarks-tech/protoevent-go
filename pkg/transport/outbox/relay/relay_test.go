package relay_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/quarks-tech/protoevent-go/pkg/transport/outbox/relay"
)

// fullLeaderStore implements the whole relay.LeaderStore contract.
type fullLeaderStore struct{}

func (fullLeaderStore) TryAcquireLeaderLock(context.Context, string, string, time.Duration) (bool, error) {
	return true, nil
}

func (fullLeaderStore) ReleaseLeaderLock(context.Context, string, string) error { return nil }

// acquireOnlyStore is the v1-shaped store: it satisfied the single-method
// LeaderStore of the previous release and silently stopped satisfying the
// widened one.
type acquireOnlyStore struct{}

func (acquireOnlyStore) TryAcquireLeaderLock(context.Context, string, string, time.Duration) (bool, error) {
	return true, nil
}

// releaseOnlyStore stands in for a signature that drifted on the acquire side.
type releaseOnlyStore struct{}

func (releaseOnlyStore) ReleaseLeaderLock(context.Context, string, string) error { return nil }

// driftedLeaderStore has BOTH method names but neither signature — a store
// refactored to take a config struct, say. Narrower probe interfaces match none of
// it, so a name-based probe is the only thing that catches it.
type driftedLeaderStore struct{}

type leaderLockRequest struct {
	Name     string
	HolderID string
	TTL      time.Duration
}

func (driftedLeaderStore) TryAcquireLeaderLock(context.Context, leaderLockRequest) (bool, error) {
	return true, nil
}

func (driftedLeaderStore) ReleaseLeaderLock(context.Context, leaderLockRequest) error { return nil }

// pointerReceiverLeaderStore implements the full contract on its POINTER type but
// is passed by value, so the value's method set is empty. Neither the interface
// assertion nor a value-only name probe sees it.
type pointerReceiverLeaderStore struct{}

func (*pointerReceiverLeaderStore) TryAcquireLeaderLock(context.Context, string, string, time.Duration) (bool, error) {
	return true, nil
}

func (*pointerReceiverLeaderStore) ReleaseLeaderLock(context.Context, string, string) error {
	return nil
}

// noLeaderStore implements neither half: the documented single-instance mode.
type noLeaderStore struct{}

// TestCheckLeaderStoreDetectsPartialImplementations pins the capability probe.
// Leadership support is discovered by type assertion, and a miss is a legitimate
// mode (always-leader), so a store that MEANT to be a LeaderStore — one method
// renamed, a signature drifted, or a method added to the interface after the
// store was written — used to degrade to leader.nopLeaderStore, which grants
// every acquire. Every replica then believed it was leader: each event delivered
// once per pod and, because concurrent drains commit GREATEST offsets, some
// events skipped entirely — with one Info line as the only trace. A
// half-implemented capability is a bug, so it must be rejected at construction.
func TestCheckLeaderStoreDetectsPartialImplementations(t *testing.T) {
	t.Run("complete", func(t *testing.T) {
		ls, err := relay.CheckLeaderStore(fullLeaderStore{})
		if err != nil {
			t.Fatalf("CheckLeaderStore: %v", err)
		}
		if ls == nil {
			t.Fatal("CheckLeaderStore returned a nil LeaderStore for a complete implementation")
		}
	})

	t.Run("complete via pointer", func(t *testing.T) {
		// The ordinary way a store is written: methods on the pointer, passed as a
		// pointer. Must resolve, not be mistaken for a partial implementation.
		ls, err := relay.CheckLeaderStore(&pointerReceiverLeaderStore{})
		if err != nil {
			t.Fatalf("CheckLeaderStore: %v", err)
		}
		if ls == nil {
			t.Fatal("CheckLeaderStore returned a nil LeaderStore for a pointer implementation")
		}
	})

	t.Run("none", func(t *testing.T) {
		ls, err := relay.CheckLeaderStore(noLeaderStore{})
		if err != nil {
			t.Fatalf("CheckLeaderStore: %v, want nil (always-leader is a documented mode)", err)
		}
		if ls != nil {
			t.Fatal("CheckLeaderStore returned a LeaderStore for a store implementing none of it")
		}
	})

	partials := map[string]any{
		"acquire only":                acquireOnlyStore{},
		"release only":                releaseOnlyStore{},
		"both signatures drifted":     driftedLeaderStore{},
		"pointer receivers, by value": pointerReceiverLeaderStore{},
	}
	for name, store := range partials {
		t.Run(name, func(t *testing.T) {
			ls, err := relay.CheckLeaderStore(store)
			if !errors.Is(err, relay.ErrPartialLeaderStore) {
				t.Fatalf("CheckLeaderStore error = %v, want ErrPartialLeaderStore", err)
			}
			if ls != nil {
				t.Fatal("CheckLeaderStore returned a LeaderStore alongside an error")
			}
		})
	}
}
