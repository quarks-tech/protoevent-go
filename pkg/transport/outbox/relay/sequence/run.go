package sequence

import (
	"context"
	"errors"
	"time"

	"github.com/quarks-tech/protoevent-go/pkg/transport/outbox"
)

// commitTimeout bounds the final CommitOffset on a planned shutdown with a
// fresh context.Background(): that commit runs after the run ctx is already
// canceled, and a real store would fail the write on a dead context —
// mirroring relay/leader.go's releaseTimeout pattern.
const commitTimeout = 5 * time.Second

// errLostLeadership signals that a between-pages lease renewal reported the
// lock lost or failed: the current pass must stop as a whole (a known
// non-leader draining another page would duplicate it, and its sweep would
// run without the lease). RunOnce maps it to a clean nil stop — losing
// leadership is not an error; a persistent leader-store error resurfaces via
// the next tick's opening TryAcquire.
var errLostLeadership = errors.New("sequence: leadership lost mid-pass")

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
			r.options.Logger.Error("sequence relay: pass failed", "relay", r.name, "err", err)
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
				r.options.Logger.Error("sequence relay: pass failed", "relay", r.name, "err", err)
			}
		}
	}
}

// RunOnce performs one tick: acquire leadership, sequence, drain, and (on
// cadence) sweep. Rows sequenced this tick are drained this tick because the
// sequencer pass commits before the drain query runs. The lease acquired here
// is additionally renewed between pages inside sequence and drain, so a pass
// over a long backlog cannot outlive it. Losing leadership at one of those
// renewals stops the whole pass cleanly (nil): the remaining phases are
// skipped rather than run as a known non-leader.
func (r *Relay) RunOnce(ctx context.Context) error {
	isLeader, err := r.leader.TryAcquire(ctx)
	if err != nil {
		return err
	}
	if !isLeader {
		return nil
	}

	if err := r.sequence(ctx); err != nil {
		if errors.Is(err, errLostLeadership) {
			return nil // clean stop: losing leadership is not an error
		}
		return err
	}
	if err := r.drain(ctx); err != nil {
		if errors.Is(err, errLostLeadership) {
			return nil
		}
		return err
	}
	return r.maybeSweep(ctx)
}

// sequence assigns offsets to committed pending rows, looping while pages are
// full so a burst is fully sequenced within the tick. The leader lease is
// renewed between full pages (see drain for the rationale).
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
		// Full page: renew the lease before the next pass. A renewal that
		// reports the lock lost — or fails outright — stops the whole pass
		// via errLostLeadership: a known non-leader must not keep sequencing,
		// draining, or sweeping. RunOnce maps the sentinel to a clean nil
		// stop; a persistent leader-store error resurfaces via the next
		// tick's opening TryAcquire.
		if isLeader, err := r.leader.TryAcquire(ctx); err != nil || !isLeader {
			return errLostLeadership
		}
	}
}

// drain forwards messages in Seq order, committing the offset only after a
// successful send. On send failure it stops the lane (default) or parks and
// continues (WithErrorHandler). Loops while pages are full, renewing the
// leader lease between pages so a long backlog cannot outlive the lease —
// bounding stale-leader overlap to a single page.
func (r *Relay) drain(ctx context.Context) error {
	offset, err := r.store.Offset(ctx, r.name)
	if err != nil {
		return err
	}
	if offset == 0 && !r.options.StartFromBeginning && !r.offsetInitialized {
		// A brand-new group starts at "latest" (parity with the stream
		// runtime's start-at-now) unless the caller opted into a full replay.
		// InitOffsetLatest is insert-if-absent, so re-running it against an
		// existing row — even one committed at 0 — is harmless: an existing
		// row is never modified. The latch below only saves the round-trip.
		offset, err = r.store.InitOffsetLatest(ctx, r.name)
		if err != nil {
			return err
		}
		r.offsetInitialized = true
	}

	for {
		msgs, listErr := r.store.ListMessages(ctx, offset, r.options.BatchSize)
		var poison *DecodeError
		if listErr != nil && !errors.As(listErr, &poison) {
			return listErr
		}
		if len(msgs) == 0 && poison == nil {
			return nil
		}

		maxSeq := offset
		sent := 0
		stopped := false
		canceled := false
		for _, m := range msgs {
			// A canceled run context is a shutdown, not a message fault: stop
			// the lane before touching the next message, so a canceled ctx
			// can't walk the rest of the page fail-fast (and, with an
			// ErrorHandler, park healthy messages).
			if ctx.Err() != nil {
				canceled = true
				break
			}
			if sendErr := r.sender.Send(ctx, m.Metadata, m.Data); sendErr != nil {
				if ctx.Err() != nil {
					// The send failed because the run context was canceled
					// mid-flight: same shutdown case — no park, no advance.
					// A genuine op-level error (e.g. DeadlineExceeded while
					// the run ctx is alive) falls through to handleError.
					canceled = true
					break
				}
				r.handleError(ctx, m, sendErr)
				if r.options.ErrorHandler == nil {
					stopped = true
					break // stop-the-lane: leave this seq for the next tick
				}
				// park-and-continue: advance past the parked message. It is
				// reported via ObserveError (in handleError) and NOT counted
				// as sent.
				maxSeq = m.Seq
				continue
			}
			maxSeq = m.Seq
			sent++
		}

		// A poison row (persisted metadata failed to decode) sits right after
		// the decoded prefix. With an ErrorHandler it is parked like any other
		// failed message and the lane advances past it; without one the lane
		// stops at it (at-least-once, order preserved). Skipped on shutdown
		// and when the lane already stopped before reaching it.
		parkedPoison := false
		if poison != nil && !stopped && !canceled && r.options.ErrorHandler != nil && ctx.Err() == nil {
			r.handleError(ctx, &outbox.Message{ID: poison.ID, Seq: poison.Seq}, poison)
			maxSeq = max(maxSeq, poison.Seq)
			parkedPoison = true
		}

		if maxSeq > offset {
			if err := r.commitOffset(ctx, maxSeq); err != nil {
				return err
			}
			// A single leader owns the watermark, so the value we just
			// committed IS the new offset: advance the local variable instead
			// of re-querying Offset every iteration (a wasted round-trip per
			// full page).
			offset = maxSeq
		}

		full := len(msgs) == r.options.BatchSize
		if len(msgs) > 0 || parkedPoison {
			// `sent` counts successful sends only; parked messages are
			// reported via ObserveError and excluded (relay.Observer contract).
			// A poison-only page (empty decoded prefix, poison parked) still
			// disposed of a message, so it is observed too — with a zero
			// oldestAge, since there is no decoded row to anchor the lag on.
			more := stopped || canceled || full || poison != nil
			oldestAge := time.Duration(0)
			if len(msgs) > 0 {
				oldestAge = time.Since(msgs[0].CreateTime)
			}
			r.options.Observer.ObserveDrained(r.name, sent, oldestAge, more)
		}

		switch {
		case canceled:
			// Shutdown: successes are already committed above (the final
			// commit goes through commitOffset's fresh bounded context — the
			// run ctx is already dead here); Run's pass-level quieting keeps
			// this silent.
			return ctx.Err()
		case stopped:
			return nil
		case poison != nil && !parkedPoison:
			// No ErrorHandler: what succeeded is committed; surface the decode
			// failure and stop the lane at the poison row.
			return listErr
		case !full && !parkedPoison:
			return nil
		}

		// Full page (or a parked poison row with possibly more behind it):
		// another page follows. Renew the lease first — without this a long
		// backlog could outlast LeaseTTL and the whole drain would run
		// concurrently with a new leader. A renewal that reports the lock
		// lost — or fails outright — stops the whole pass via
		// errLostLeadership: what is already committed stands, and RunOnce
		// skips the sweep instead of running it as a known non-leader; a
		// persistent leader-store error resurfaces via the next tick's
		// opening TryAcquire.
		if err := ctx.Err(); err != nil {
			return err
		}
		if isLeader, err := r.leader.TryAcquire(ctx); err != nil || !isLeader {
			return errLostLeadership
		}
	}
}

// commitOffset persists the watermark. When the run ctx is already canceled
// (the final commit on a planned shutdown), the store call gets a fresh
// bounded context instead: a real store fails writes on a dead context, and
// losing that commit would redeliver the page's acknowledged sends on
// restart. Behavior on a live ctx is unchanged.
func (r *Relay) commitOffset(ctx context.Context, seq int64) error {
	if ctx.Err() != nil {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(context.Background(), commitTimeout)
		defer cancel()
	}
	return r.store.CommitOffset(ctx, r.name, seq)
}

func (r *Relay) maybeSweep(ctx context.Context) error {
	if r.retention == nil {
		return nil
	}
	r.tickCount++
	if r.tickCount%r.options.RetentionSweepEvery != 0 {
		return nil
	}
	before := time.Now().UTC().Add(-r.options.RetentionWindow)
	_, err := r.retention.SweepMessages(ctx, before, r.options.RetentionSweepBatch)
	return err
}

// handleError routes a genuine per-message failure (send or decode) to the
// ErrorHandler (if configured), the Observer, and the Logger. Never called
// for shutdown cancellation: a canceled run context stops the lane instead.
func (r *Relay) handleError(ctx context.Context, msg *outbox.Message, err error) {
	if r.options.ErrorHandler != nil {
		r.options.ErrorHandler(ctx, msg, err)
	}
	r.options.Observer.ObserveError(r.name, err)
	r.options.Logger.Error("sequence relay: message failed", "relay", r.name, "event_id", msg.ID, "err", err)
}
