package sequence

import (
	"context"
	"time"

	"github.com/quarks-tech/protoevent-go/pkg/transport/outbox"
	"github.com/quarks-tech/protoevent-go/pkg/transport/outbox/relay"
)

const releaseTimeout = 5 * time.Second

// Run drives the relay until ctx is canceled, then releases leadership so a
// planned shutdown fails over in well under LeaseTTL.
func (r *Relay) Run(ctx context.Context) error {
	ticker := time.NewTicker(r.options.PollInterval)
	defer ticker.Stop()
	defer r.releaseLeadership()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if err := r.RunOnce(ctx); err != nil {
				r.options.Observer.ObserveError(r.name, err)
				if r.options.Logger != nil {
					r.options.Logger.Errorf("relay %q: %v", r.name, err)
				}
			}
		}
	}
}

// RunOnce performs one tick: acquire leadership, sequence, drain, and (on
// cadence) sweep. Rows sequenced this tick are drained this tick because the
// sequencer pass commits before the drain query runs.
func (r *Relay) RunOnce(ctx context.Context) error {
	isLeader, err := r.tryAcquireLeadership(ctx)
	if err != nil {
		return err
	}
	if !isLeader {
		return nil
	}

	if err := r.sequence(ctx); err != nil {
		return err
	}
	if err := r.drain(ctx); err != nil {
		return err
	}
	return r.maybeSweep(ctx)
}

func (r *Relay) tryAcquireLeadership(ctx context.Context) (bool, error) {
	ls, ok := r.store.(relay.LeaderStore)
	if !ok {
		r.isLeader = true
		return true, nil
	}
	held, err := ls.TryAcquireLeaderLock(ctx, r.options.LeaderLockName, r.holderID, r.options.LeaseTTL)
	if err != nil {
		return false, err
	}
	r.isLeader = held
	return held, nil
}

func (r *Relay) releaseLeadership() {
	ls, ok := r.store.(relay.LeaderStore)
	if !ok || !r.isLeader {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), releaseTimeout)
	defer cancel()
	_ = ls.ReleaseLeaderLock(ctx, r.options.LeaderLockName, r.holderID)
	r.isLeader = false
}

// sequence assigns offsets to committed pending rows, looping while pages are
// full so a burst is fully sequenced within the tick.
func (r *Relay) sequence(ctx context.Context) error {
	ss, ok := r.store.(SequencerStore)
	if !ok || r.options.DisableSequencer {
		return nil
	}
	for {
		n, err := ss.SequenceMessages(ctx, r.options.SequenceBatchSize)
		if err != nil {
			return err
		}
		r.options.Observer.ObserveSequenced(r.name, n)
		if n < r.options.SequenceBatchSize {
			return nil
		}
		if err := ctx.Err(); err != nil {
			return err
		}
	}
}

// drain forwards messages in Seq order, committing the offset only after a
// successful send. On send failure it stops the lane (default) or parks and
// continues (WithErrorHandler). Loops while pages are full.
func (r *Relay) drain(ctx context.Context) error {
	for {
		offset, err := r.store.Offset(ctx, r.name)
		if err != nil {
			return err
		}
		msgs, err := r.store.ListMessages(ctx, offset, r.options.BatchSize)
		if err != nil {
			return err
		}
		if len(msgs) == 0 {
			return nil
		}

		maxSeq := offset
		processed := 0
		stopped := false
		for _, m := range msgs {
			if sendErr := r.sender.Send(ctx, m.Metadata, m.Data); sendErr != nil {
				r.handleError(ctx, m, sendErr)
				if r.options.ErrorHandler == nil {
					stopped = true
					break // stop-the-lane: leave this seq for the next tick
				}
				// park-and-continue: advance past the parked message
			}
			maxSeq = m.Seq
			processed++
		}

		if maxSeq > offset {
			if err := r.store.CommitOffset(ctx, r.name, maxSeq); err != nil {
				return err
			}
		}

		full := len(msgs) == r.options.BatchSize
		r.options.Observer.ObserveDrained(r.name, processed, time.Since(msgs[0].CreateTime), full && !stopped)

		if stopped || !full {
			return nil
		}
		if err := ctx.Err(); err != nil {
			return err
		}
	}
}

func (r *Relay) maybeSweep(ctx context.Context) error {
	if r.options.RetentionWindow <= 0 || r.options.RetentionSweepEvery <= 0 {
		return nil
	}
	rs, ok := r.store.(RetentionStore)
	if !ok {
		return nil
	}
	r.tickCount++
	if r.tickCount%r.options.RetentionSweepEvery != 0 {
		return nil
	}
	before := time.Now().Add(-r.options.RetentionWindow)
	_, err := rs.SweepMessages(ctx, before, r.options.RetentionSweepBatch)
	return err
}

func (r *Relay) handleError(ctx context.Context, msg *outbox.Message, err error) {
	if r.options.ErrorHandler != nil {
		r.options.ErrorHandler(ctx, msg, err)
	}
	r.options.Observer.ObserveError(r.name, err)
	if r.options.Logger != nil {
		r.options.Logger.Errorf("relay %q: send message %s: %v", r.name, msg.ID, err)
	}
}
