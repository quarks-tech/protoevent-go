package sequence

import (
	"context"
	"errors"
	"time"

	"github.com/quarks-tech/protoevent-go/pkg/transport/outbox"
	"github.com/quarks-tech/protoevent-go/pkg/transport/outbox/internal/relayutil"
	"github.com/quarks-tech/protoevent-go/pkg/transport/outbox/relay"
)

// commitTimeout bounds the final CommitOffset on a planned shutdown with a
// fresh context.Background(): that commit runs after the run ctx is already
// canceled, and a real store would fail the write on a dead context —
// mirroring relay/leader.go's releaseTimeout pattern.
const commitTimeout = 5 * time.Second

// maxPagesPerTick bounds the full-page loops in sequence() and drain() within
// one RunOnce. Without a cap, a producer that keeps every page full pins the
// pass in one phase indefinitely — sequence() would starve drain() (nothing
// delivered at all while the sequencer treadmills) and both would starve the
// retention sweep. Hitting the cap ends the phase cleanly; the next tick
// continues where the offset/sequencer left off. Generous on purpose: at the
// default page sizes this is 64k rows sequenced / 6.4k sends per tick.
const maxPagesPerTick = 64

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
	defer func() {
		// Informational only: failover falls back to TTL expiry, and a store
		// outage also surfaces on the successor's TryAcquire (see Release doc).
		if err := r.leader.Release(); err != nil {
			r.options.Logger.Warn("sequence relay: release leader lock", "relay", r.name, "err", err)
		}
	}()

	// Do one pass immediately instead of waiting a full PollInterval for the
	// first tick, so a freshly started relay starts delivering right away.
	if ctx.Err() != nil {
		return context.Cause(ctx)
	}
	if err := r.RunOnce(ctx); err != nil {
		if ctx.Err() == nil {
			// Only observe if this isn't a planned shutdown (see the
			// same-shaped handling in the loop below for the rationale).
			relayutil.ObserveError(r.options.Observer, r.name, err)
			r.options.Logger.Error("sequence relay: pass failed", "relay", r.name, "err", err)
		}
	}

	for {
		select {
		case <-ctx.Done():
			// context.Cause degrades to ctx.Err() under a plain WithCancel and
			// surfaces the richer cancel reason under WithCancelCause/errgroup.
			return context.Cause(ctx)
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
				relayutil.ObserveError(r.options.Observer, r.name, err)
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
// full so a burst is fully sequenced within the tick — bounded by
// maxPagesPerTick so a sustained full-rate producer cannot pin the pass here
// and starve drain and the sweep. The leader lease is renewed between full
// pages (see drain for the rationale).
func (r *Relay) sequence(ctx context.Context) error {
	if r.sequencer == nil {
		return nil
	}
	for range maxPagesPerTick {
		n, err := r.sequencer.SequenceMessages(ctx, r.options.SequenceBatchSize)
		if err != nil {
			return err
		}
		if n > 0 {
			// Match drain's idle behavior: don't report a signal for an idle
			// pass that sequenced nothing.
			relayutil.ObserveSequenced(r.options.Observer, r.name, n)
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
	return nil // maxPagesPerTick full pages: yield to drain; the next tick continues
}

// drain forwards messages in Seq order, committing the offset only after a
// successful send. A send failure ALWAYS stops the lane — the failed message
// retries next tick, preserving both order and delivery. Only a poison row
// (persisted metadata that fails to decode, *DecodeError) is ever parked via
// the ErrorHandler: retrying it can never succeed. Loops while pages are full
// (bounded by maxPagesPerTick so a sustained backlog cannot starve the sweep),
// renewing the leader lease between pages so a long backlog cannot outlive
// the lease — bounding stale-leader overlap to a single page.
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

	for range maxPagesPerTick {
		msgs, listErr := r.store.ListMessages(ctx, offset, r.options.BatchSize)
		poison, isPoison := errors.AsType[*DecodeError](listErr)
		if listErr != nil && !isPoison {
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
					canceled = true
					break
				}
				// A send failure ALWAYS stops the lane, ErrorHandler or not:
				// it is downstream trouble (broker down, timeout), not a
				// message fault, and parking healthy messages during an
				// outage would bulk-divert the entire backlog to the DLQ
				// while permanently advancing the offset past it. The failed
				// message retries next tick — order and delivery preserved.
				// (The nil handler keeps it out of the DLQ; observe+log only.)
				r.handleError(ctx, nil, m, sendErr)
				stopped = true
				break // stop-the-lane: leave this seq for the next tick
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
			r.handleError(ctx, r.options.ErrorHandler, &outbox.Message{ID: poison.ID, Seq: poison.Seq}, poison)
			maxSeq = max(maxSeq, poison.Seq)
			parkedPoison = true
		}

		if maxSeq > offset {
			// On a planned shutdown this commit still lands: boundedStore's
			// CommitOffset detaches from the dead run ctx (see bounded.go).
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
			relayutil.ObserveDrained(r.options.Observer, r.name, sent, oldestAge, more)
		}

		switch {
		case canceled:
			// Shutdown: successes are already committed above (the final
			// commit goes through commitOffset's fresh bounded context — the
			// run ctx is already dead here); Run's pass-level quieting keeps
			// this silent.
			return context.Cause(ctx)
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
	return nil // maxPagesPerTick full pages: yield to the sweep; the next tick continues
}

// maybeSweep runs the retention sweep at most once per RetentionSweepInterval
// of wall-clock time — decoupled from PollInterval, so retuning the tick does
// not silently change sweep cadence. The first leader tick sweeps immediately
// (lastSweep zero); the sweep is bounded by RetentionSweepBatch, so an early
// first sweep is cheap.
func (r *Relay) maybeSweep(ctx context.Context) error {
	if r.retention == nil {
		return nil
	}
	if !r.lastSweep.IsZero() && time.Since(r.lastSweep) < r.options.RetentionSweepInterval {
		return nil
	}
	r.lastSweep = time.Now()
	before := time.Now().UTC().Add(-r.options.RetentionWindow)
	_, err := r.retention.SweepMessages(ctx, before, r.options.RetentionSweepBatch)
	return err
}

// handleError routes a genuine per-message failure to the shared relay error
// policy. h is the configured ErrorHandler for parkable poison rows and nil
// for stop-the-lane send failures (observe+log only — the message will be
// retried, not parked). Never called for shutdown cancellation: a canceled
// run context stops the lane instead.
func (r *Relay) handleError(ctx context.Context, h relay.ErrorHandler, msg *outbox.Message, err error) {
	relayutil.HandleMessageError(ctx, h, r.options.Observer, r.options.Logger, "sequence", r.name, msg, err)
}
