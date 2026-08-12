package sequence_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/quarks-tech/protoevent-go/pkg/transport/outbox"
	"github.com/quarks-tech/protoevent-go/pkg/transport/outbox/relay/sequence"
)

// leaderlessStore implements sequence.Store (and Sequencer) but deliberately not
// relay.LeaderStore. It delegates to fakeStore rather than embedding it, because
// embedding would promote the lock methods and make it a LeaderStore again.
type leaderlessStore struct{ inner *fakeStore }

func newLeaderlessStore() leaderlessStore { return leaderlessStore{inner: newFakeStore()} }

func (s leaderlessStore) ListMessages(ctx context.Context, afterSeq int64, limit int) ([]*outbox.Message, error) {
	return s.inner.ListMessages(ctx, afterSeq, limit)
}

func (s leaderlessStore) Offset(ctx context.Context, name string) (int64, bool, error) {
	return s.inner.Offset(ctx, name)
}

func (s leaderlessStore) CommitOffset(ctx context.Context, name string, seq int64) error {
	return s.inner.CommitOffset(ctx, name, seq)
}

func (s leaderlessStore) InitOffsetLatest(ctx context.Context, name string) (int64, error) {
	return s.inner.InitOffsetLatest(ctx, name)
}

func (s leaderlessStore) SequenceMessages(ctx context.Context, limit int) (int, error) {
	return s.inner.SequenceMessages(ctx, limit)
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
// capability is never inferred.
//
// Leadership is discovered by type assertion, and a miss used to be a legitimate
// mode (always-leader) announced by one Info line. A store that MEANT to elect —
// one method renamed, a signature drifted, a method added to the interface after
// the store was written — therefore degraded to "every acquire granted". Every
// replica then believed it was leader: each event delivered once per pod and,
// because concurrent drains commit GREATEST offsets, some events skipped
// entirely. Single-instance mode must be stated, not fallen into.
func TestNewRelayRequiresLeaderStoreOrExplicitWaiver(t *testing.T) {
	t.Run("store cannot elect", func(t *testing.T) {
		_, err := sequence.NewRelay("c", newLeaderlessStore(), noopSender)
		if err == nil {
			t.Fatal("NewRelay succeeded over a store with no leader election; want an error naming the waiver")
		}
		if !strings.Contains(err.Error(), "WithoutLeaderElection") {
			t.Fatalf("error = %v, want it to name WithoutLeaderElection", err)
		}
	})

	t.Run("both lock signatures drifted", func(t *testing.T) {
		_, err := sequence.NewRelay("c", driftedLeaderStore{newLeaderlessStore()}, noopSender)
		if err == nil {
			t.Fatal("NewRelay succeeded over a store whose lock methods drifted; " +
				"it would run every replica as leader")
		}
	})

	t.Run("waived", func(t *testing.T) {
		r, err := sequence.NewRelay("c", newLeaderlessStore(), noopSender, sequence.WithoutLeaderElection())
		if err != nil {
			t.Fatalf("NewRelay with WithoutLeaderElection: %v", err)
		}
		// The waiver must actually run the relay, not just construct it: a
		// single-instance relay is always leader and drains normally.
		if err := r.RunOnce(t.Context()); err != nil {
			t.Fatalf("RunOnce in single-instance mode: %v", err)
		}
	})

	t.Run("waived over a store that could elect", func(t *testing.T) {
		// The waiver wins: an operator running one instance may opt out of the
		// lock entirely even when the store supports it.
		if _, err := sequence.NewRelay("c", newFakeStore(), noopSender, sequence.WithoutLeaderElection()); err != nil {
			t.Fatalf("NewRelay: %v", err)
		}
	})
}
