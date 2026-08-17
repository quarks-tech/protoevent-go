package sequence

import (
	"context"
	"time"

	"github.com/quarks-tech/protoevent-go/pkg/transport/outbox"
	"github.com/quarks-tech/protoevent-go/pkg/transport/outbox/internal/bound"
)

// boundedStore decorates the relay's Store with the shared operation-timeout
// policy (internal/bound): every call carries the OpTimeout bound, and the two
// writes that record already-completed work are additionally detached from the
// run context's cancellation.
//
// The bound is applied by decoration at construction (NewRelay), not by per-call
// helpers in the run loop: the policy lives in one type, and a Store method
// added later is bounded by construction instead of by remembering a wrapper.
// leader.Elector.TryAcquire self-bounds the same way. Each call is one page/row
// of work (BatchSize, SequenceBatchSize, or a single-row upsert), so a healthy
// operation never approaches the bound.
type boundedStore struct {
	inner Store
	ttl   time.Duration
}

func (s boundedStore) ListMessages(ctx context.Context, afterSeq int64, limit int) ([]*outbox.Message, error) {
	ctx, cancel := bound.Call(ctx, s.ttl)
	defer cancel()

	return s.inner.ListMessages(ctx, afterSeq, limit)
}

func (s boundedStore) Offset(ctx context.Context, name string) (int64, bool, error) {
	ctx, cancel := bound.Call(ctx, s.ttl)
	defer cancel()

	return s.inner.Offset(ctx, name)
}

func (s boundedStore) InitOffsetLatest(ctx context.Context, name string) (int64, error) {
	ctx, cancel := bound.Call(ctx, s.ttl)
	defer cancel()

	return s.inner.InitOffsetLatest(ctx, name)
}

// CommitOffset uses bound.Commit: the commit records sends that already
// happened, so losing it to a canceled run context would redeliver the page's
// acknowledged sends on restart. The consumer test suite pins the detachment
// (commitHadDeadline/commitCtxErr).
func (s boundedStore) CommitOffset(ctx context.Context, name string, seq int64) error {
	ctx, cancel := bound.Commit(ctx, s.ttl)
	defer cancel()

	return s.inner.CommitOffset(ctx, name, seq)
}

// boundedSequencer, boundedRetention and boundedClock apply the same policy to
// the optional store capabilities. They are separate types rather than fields on
// one decorator because the relay holds each capability in its own interface
// field, where nil means "the store does not have it".
type boundedSequencer struct {
	inner Sequencer
	ttl   time.Duration
}

func (s boundedSequencer) SequenceMessages(ctx context.Context, limit int) (int, error) {
	ctx, cancel := bound.Call(ctx, s.ttl)
	defer cancel()

	return s.inner.SequenceMessages(ctx, limit)
}

type boundedRetention struct {
	inner Sweeper
	ttl   time.Duration
}

func (s boundedRetention) SweepMessages(ctx context.Context, limit int) (int, error) {
	ctx, cancel := bound.Call(ctx, s.ttl)
	defer cancel()

	return s.inner.SweepMessages(ctx, limit)
}

type boundedClock struct {
	inner Clock
	ttl   time.Duration
}

// StoreNow uses bound.Salvaged: drain reports its pass — and so reads the clock
// for the lag value — AFTER it has already observed run-ctx cancellation, and on
// a dead context a real store fails the query immediately, degrading lag to the
// host-clock fallback on every planned shutdown and warning about a clock that
// is fine. Unlike a commit this records nothing, so a cancel arriving mid-call
// still aborts it.
func (s boundedClock) StoreNow(ctx context.Context) (time.Time, error) {
	ctx, cancel := bound.Salvaged(ctx, s.ttl)
	defer cancel()

	return s.inner.StoreNow(ctx)
}
