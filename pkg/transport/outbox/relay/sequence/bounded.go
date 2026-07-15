package sequence

import (
	"context"
	"time"

	"github.com/quarks-tech/protoevent-go/pkg/transport/outbox"
	"github.com/quarks-tech/protoevent-go/pkg/transport/outbox/internal/relayutil"
)

// boundedStore decorates the relay's Store with a per-operation LeaseTTL
// bound — parity with the stream runtime, and for the same reason:
// database/sql has no default operation timeout, so a connection that wedges
// at the network layer would otherwise block the single Run goroutine
// indefinitely — the lease silently expires, a standby takes over, and this
// instance neither errors nor logs, then resumes as a stale leader when the
// call finally unblocks. Past LeaseTTL the call's success is moot anyway: the
// lease is already lost. Each call is one page/row of work (BatchSize,
// SequenceBatchSize, or a single-row upsert), so a healthy operation never
// approaches the bound.
//
// The bound is applied by decoration at construction (NewRelay), not by
// per-call helpers in the run loop: the policy lives in one type, and a Store
// method added later is bounded by construction instead of by remembering a
// wrapper. LeaderElector.TryAcquire self-bounds the same way.
type boundedStore struct {
	inner Store
	ttl   time.Duration
}

func (s boundedStore) ListMessages(ctx context.Context, afterSeq int64, limit int) ([]*outbox.Message, error) {
	ctx, cancel := context.WithTimeout(ctx, s.ttl)
	defer cancel()
	return s.inner.ListMessages(ctx, afterSeq, limit)
}

func (s boundedStore) Offset(ctx context.Context, name string) (int64, error) {
	ctx, cancel := context.WithTimeout(ctx, s.ttl)
	defer cancel()
	return s.inner.Offset(ctx, name)
}

func (s boundedStore) InitOffsetLatest(ctx context.Context, name string) (int64, error) {
	ctx, cancel := context.WithTimeout(ctx, s.ttl)
	defer cancel()
	return s.inner.InitOffsetLatest(ctx, name)
}

// CommitOffset is additionally shutdown-safe: once the run ctx is canceled
// (the final commit on a planned shutdown), the store call gets a
// values-preserving detached context bounded by commitTimeout instead — a
// real store fails writes on a dead context, and losing that commit would
// redeliver the page's acknowledged sends on restart.
func (s boundedStore) CommitOffset(ctx context.Context, name string, seq int64) error {
	ctx, cancel := relayutil.BoundContext(ctx, s.ttl, commitTimeout)
	defer cancel()
	return s.inner.CommitOffset(ctx, name, seq)
}

// boundedSequencer and boundedRetention apply the same LeaseTTL bound to the
// optional capabilities (see boundedStore).
type boundedSequencer struct {
	inner SequencerStore
	ttl   time.Duration
}

func (s boundedSequencer) SequenceMessages(ctx context.Context, limit int) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, s.ttl)
	defer cancel()
	return s.inner.SequenceMessages(ctx, limit)
}

type boundedRetention struct {
	inner RetentionStore
	ttl   time.Duration
}

func (s boundedRetention) SweepMessages(ctx context.Context, before time.Time, limit int) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, s.ttl)
	defer cancel()
	return s.inner.SweepMessages(ctx, before, limit)
}
