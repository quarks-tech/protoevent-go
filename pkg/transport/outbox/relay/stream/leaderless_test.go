package stream_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/quarks-tech/protoevent-go/pkg/event"
	"github.com/quarks-tech/protoevent-go/pkg/transport/outbox/relay/stream"
)

// leaderlessStore implements stream.Store but deliberately not relay.LeaderStore.
// It delegates to fakeStore rather than embedding it, because embedding would
// promote the lock methods and make it a LeaderStore again.
type leaderlessStore struct{ inner *fakeStore }

func newLeaderlessStore() leaderlessStore {
	return leaderlessStore{inner: &fakeStore{stream: &fakeStream{}}}
}

func (s leaderlessStore) LoadToken(ctx context.Context, name string) (string, time.Time, error) {
	return s.inner.LoadToken(ctx, name)
}

func (s leaderlessStore) SaveToken(ctx context.Context, name, tok string, ct time.Time) (bool, error) {
	return s.inner.SaveToken(ctx, name, tok, ct)
}

func (s leaderlessStore) Watch(ctx context.Context, token string, maxAwait time.Duration) (stream.Stream, error) {
	return s.inner.Watch(ctx, token, maxAwait)
}

// driftedLeaderStore carries BOTH lock method NAMES but neither signature — a
// store refactored to take a config struct, say. It is the case a
// narrower-interface probe cannot catch, and the one the relay must not mistake
// for a deliberate single-instance deployment.
type driftedLeaderStore struct{ leaderlessStore }

type leaderLockRequest struct {
	Name     string
	HolderID string
	TTL      time.Duration
}

func (driftedLeaderStore) TryAcquireLeaderLock(context.Context, leaderLockRequest) (bool, error) {
	return true, nil
}

func (driftedLeaderStore) ReleaseLeaderLock(context.Context, leaderLockRequest) error { return nil }

// TestNewRelayRequiresLeaderStoreOrExplicitWaiver pins that a missing leadership
// capability is never inferred. See the sequence runtime's twin for the failure
// this prevents: every replica believing it is leader, delivering the whole
// stream once per pod, announced by nothing louder than an Info line.
func TestNewRelayRequiresLeaderStoreOrExplicitWaiver(t *testing.T) {
	noopSender := senderFunc(func(context.Context, *event.Metadata, []byte) error { return nil })

	t.Run("store cannot elect", func(t *testing.T) {
		_, err := stream.NewRelay("c", newLeaderlessStore(), noopSender)
		if err == nil {
			t.Fatal("NewRelay succeeded over a store with no leader election; want an error naming the waiver")
		}
		if !strings.Contains(err.Error(), "WithoutLeaderElection") {
			t.Fatalf("error = %v, want it to name WithoutLeaderElection", err)
		}
	})

	t.Run("both lock signatures drifted", func(t *testing.T) {
		_, err := stream.NewRelay("c", driftedLeaderStore{newLeaderlessStore()}, noopSender)
		if err == nil {
			t.Fatal("NewRelay succeeded over a store whose lock methods drifted; " +
				"it would run every replica as leader")
		}
	})

	t.Run("waived", func(t *testing.T) {
		r, err := stream.NewRelay("c", newLeaderlessStore(), noopSender, stream.WithoutLeaderElection())
		if err != nil {
			t.Fatalf("NewRelay with WithoutLeaderElection: %v", err)
		}
		if err := r.RunOnce(t.Context()); err != nil {
			t.Fatalf("RunOnce in single-instance mode: %v", err)
		}
	})

	t.Run("waived over a store that could elect", func(t *testing.T) {
		st := &fakeStore{stream: &fakeStream{}}
		if _, err := stream.NewRelay("c", st, noopSender, stream.WithoutLeaderElection()); err != nil {
			t.Fatalf("NewRelay: %v", err)
		}
	})
}
