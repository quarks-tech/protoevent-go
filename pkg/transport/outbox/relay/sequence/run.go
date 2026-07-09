package sequence

import (
	"context"
	"time"

	"github.com/quarks-tech/protoevent-go/pkg/transport/outbox"
)

// Run drives the relay until ctx is canceled, then releases leadership so a
// planned shutdown fails over in well under LeaseTTL.
func (r *Relay) Run(ctx context.Context) error {
	ticker := time.NewTicker(r.options.PollInterval)
	defer ticker.Stop()
	defer r.leader.Release()

	// Do one pass immediately instead of waiting a full PollInterval for the
	// first tick, so a freshly started relay starts delivering right away.
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := r.RunOnce(ctx); err != nil {
		if ctx.Err() == nil {
			// Only observe if this isn't a planned shutdown (see the
			// same-shaped handling in the loop below for the rationale).
			r.options.Observer.ObserveError(r.name, err)
			r.options.Logger.Errorf("relay %q: %v", r.name, err)
		}
	}

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if err := r.RunOnce(ctx); err != nil {
				if ctx.Err() != nil {
					// Planned shutdown (ctx is genuinely done): ctx.Done() will
					// fire and exit the loop; don't report a spurious error
					// metric/log for it. Gated on run-context liveness, not
					// error identity — an op-level context.DeadlineExceeded
					// while ctx is still alive is a real, recurring timeout and
					// must be observed below.
					continue
				}
				r.options.Observer.ObserveError(r.name, err)
				r.options.Logger.Errorf("relay %q: %v", r.name, err)
			}
		}
	}
}

// RunOnce performs one tick: acquire leadership, sequence, drain, and (on
// cadence) sweep. Rows sequenced this tick are drained this tick because the
// sequencer pass commits before the drain query runs.
func (r *Relay) RunOnce(ctx context.Context) error {
	isLeader, err := r.leader.TryAcquire(ctx)
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

// sequence assigns offsets to committed pending rows, looping while pages are
// full so a burst is fully sequenced within the tick.
func (r *Relay) sequence(ctx context.Context) error {
	if r.sequencer == nil {
		return nil
	}
	for {
		n, err := r.sequencer.SequenceMessages(ctx, r.options.SequenceBatchSize)
		if err != nil {
			return err
		}
		if n > 0 {
			// Match drain's idle behavior: don't report a signal for an idle
			// pass that sequenced nothing.
			r.options.Observer.ObserveSequenced(r.name, n)
		}
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
	offset, err := r.store.Offset(ctx, r.name)
	if err != nil {
		return err
	}
	if offset == 0 && !r.options.StartFromBeginning && !r.offsetInitialized {
		// offset==0 reliably means "no committed offset row": CommitOffset is
		// only ever called with seq>=1. Initialize a brand-new group at
		// "latest" (parity with the stream runtime's start-at-now) unless the
		// caller opted into a full replay.
		offset, err = r.store.InitOffsetLatest(ctx, r.name)
		if err != nil {
			return err
		}
		r.offsetInitialized = true
	}

	for {
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
			// A single leader owns the watermark, so the value we just
			// committed IS the new offset: advance the local variable instead
			// of re-querying Offset every iteration (a wasted round-trip per
			// full page).
			offset = maxSeq
		}

		full := len(msgs) == r.options.BatchSize
		more := stopped || full
		r.options.Observer.ObserveDrained(r.name, processed, time.Since(msgs[0].CreateTime), more)

		if stopped || !full {
			return nil
		}
		if err := ctx.Err(); err != nil {
			return err
		}
	}
}

func (r *Relay) maybeSweep(ctx context.Context) error {
	if r.retention == nil {
		return nil
	}
	r.tickCount++
	if r.tickCount%r.options.RetentionSweepEvery != 0 {
		return nil
	}
	before := time.Now().Add(-r.options.RetentionWindow)
	_, err := r.retention.SweepMessages(ctx, before, r.options.RetentionSweepBatch)
	return err
}

func (r *Relay) handleError(ctx context.Context, msg *outbox.Message, err error) {
	if r.options.ErrorHandler != nil {
		r.options.ErrorHandler(ctx, msg, err)
	}
	r.options.Observer.ObserveError(r.name, err)
	r.options.Logger.Errorf("relay %q: send message %s: %v", r.name, msg.ID, err)
}
