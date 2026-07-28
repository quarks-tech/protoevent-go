package sequence

import (
	"context"
	"errors"
	"strconv"
	"time"

	"github.com/quarks-tech/protoevent-go/pkg/transport/outbox"
	"github.com/quarks-tech/protoevent-go/pkg/transport/outbox/internal/notify"
	"github.com/quarks-tech/protoevent-go/pkg/transport/outbox/relay"
)

// commitTimeout bounds the final CommitOffset on a planned shutdown with a
// fresh context.Background(): that commit runs after the run ctx is already
// canceled, and a real store would fail the write on a dead context —
// mirroring internal/leader's releaseTimeout pattern.
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
		// Graceful release ends this instance's leadership: emit the
		// transition so telemetry does not leave a dead instance marked leader.
		r.trackLeadership(false)
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
			notify.Error(r.options.Observer, r.name, err)
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
				notify.Error(r.options.Observer, r.name, err)
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
	r.trackLeadership(isLeader)
	if !isLeader {
		return nil
	}

	if err := r.sequence(ctx); err != nil {
		if errors.Is(err, errLostLeadership) {
			r.trackLeadership(false)
			return nil // clean stop: losing leadership is not an error
		}
		return err
	}
	if err := r.drain(ctx); err != nil {
		if errors.Is(err, errLostLeadership) {
			r.trackLeadership(false)
			return nil
		}
		return err
	}
	return r.maybeSweep(ctx)
}

// trackLeadership fires the OnLeadership signal + Info log on transitions
// only: without it a standby takeover or a stale leader resuming after a
// wedge leaves no trace in either instance's telemetry.
func (r *Relay) trackLeadership(isLeader bool) {
	if isLeader == r.wasLeader {
		return
	}
	r.wasLeader = isLeader
	notify.Leadership(r.options.Observer, r.options.Logger, "sequence", r.name, isLeader)
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
			notify.Sequenced(r.options.Observer, r.name, n)
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
// successful send. A send failure stops the lane — the failed message retries
// next tick, preserving both order and delivery — unless an
// UnsendableClassifier calls it permanent for that message. Rows parked via the
// PoisonHandler are those retrying can never fix: a poison row (persisted
// metadata that fails to decode, *DecodeError) and a classified-unsendable
// message. Loops while pages are full
// (bounded by maxPagesPerTick so a sustained backlog cannot starve the sweep),
// renewing the leader lease between pages so a long backlog cannot outlive
// the lease — bounding stale-leader overlap to a single page.
func (r *Relay) drain(ctx context.Context) error {
	// One store-clock read per pass, not per page (see oldestAge).
	r.passStoreNow = time.Time{}

	offset, exists, err := r.store.Offset(ctx, r.name)
	if err != nil {
		return err
	}
	// Gated on the ROW's existence, not on a "primed this process" flag and not on
	// offset == 0. Both alternatives are wrong in one direction each: a memory latch
	// makes a deleted offset row (the documented DeleteOffset decommission, applied
	// to a running group) replay the whole retained log, because the relay reads 0
	// and skips the re-prime; keying on offset == 0 instead re-primes every tick for
	// as long as a group legitimately sits at 0, which is three round trips per
	// PollInterval — including a write, or an INSERT that fails on duplicate key and
	// litters the database's error log. Existence answers the actual question.
	if !exists {
		if r.options.StartFromBeginning {
			// A replaying group must still REGISTER its offset row before it
			// reads anything: retention protection is derived from MIN(last_seq)
			// across the existing offset rows, so until this group has a row of
			// its own the sweep computes the cutoff from the other groups alone
			// and can delete the very history this group was configured to
			// replay. Committing 0 is the registration primitive — CommitOffset
			// is an insert-if-absent monotone upsert, so it creates the row at 0
			// and is a no-op against any existing row.
			if err := r.store.CommitOffset(ctx, r.name, 0); err != nil {
				return err
			}
		} else {
			// A brand-new group starts at "latest" (parity with the stream
			// runtime's start-at-now) unless the caller opted into a full replay.
			// InitOffsetLatest is insert-if-absent, so re-running it against an
			// existing row — even one committed at 0 — is harmless: an existing
			// row is never modified.
			offset, err = r.store.InitOffsetLatest(ctx, r.name)
			if err != nil {
				return err
			}
		}
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
			// PoisonHandler, park healthy messages).
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
				// A message the caller's classifier calls PERMANENTLY unsendable
				// is parked like a poison row: retrying it can never succeed, and
				// leaving it at the head of the log would wedge every event behind
				// it indefinitely. Only a confirmed park (nil) advances past it;
				// an unconfirmed one stops the lane and retries the park next
				// tick. Either way handleError has already reported this failure,
				// so the stop path below must not report it a second time —
				// double-counting OnError misrepresents the incident's size.
				if r.options.Unsendable != nil && r.options.Unsendable(sendErr) {
					parkErr := r.handleError(ctx, r.options.PoisonHandler, m, sendErr)
					if parkErr == nil {
						maxSeq = m.Seq
						r.noteSendOK(m.Seq)
						continue
					}
					// The message cannot be sent AND cannot be parked, so the lane
					// is wedged here for as long as the DLQ stays broken — the
					// escalation applies exactly as it does to a plain send
					// failure.
					r.noteStuck(m.Seq, m.ID, errors.Join(sendErr, parkErr))
					stopped = true
					break
				}
				// Any other send failure ALWAYS stops the lane, PoisonHandler or
				// not: it is downstream trouble (broker down, timeout), not a
				// message fault, and parking healthy messages during an
				// outage would bulk-divert the entire backlog to the DLQ
				// while permanently advancing the offset past it. The failed
				// message retries next tick — order and delivery preserved.
				// (The nil handler keeps it out of the DLQ; observe+log only.)
				// nil handler: return is always nil (send failures are never parked).
				_ = r.handleError(ctx, nil, m, sendErr)
				r.noteStuck(m.Seq, m.ID, sendErr)
				stopped = true
				break // stop-the-lane: leave this seq for the next tick
			}
			maxSeq = m.Seq
			sent++
			r.noteSendOK(m.Seq)
		}

		// A poison row (persisted metadata failed to decode) sits right after
		// the decoded prefix. With a PoisonHandler it is parked like any other
		// failed message and the lane advances past it; without one the lane
		// stops at it (at-least-once, order preserved). Skipped on shutdown
		// and when the lane already stopped before reaching it.
		parkedPoison := false
		var parkErr error
		if poison != nil && !stopped && !canceled && r.options.PoisonHandler != nil && ctx.Err() == nil {
			// Advance past the poison row ONLY on a confirmed park (handler
			// returned nil): committing the offset past it is irreversible,
			// and an unconfirmed DLQ write would silently skip the event
			// forever. On park failure the lane stops at the poison row —
			// same as having no handler — and the park retries next pass.
			if parkErr = r.handleError(ctx, r.options.PoisonHandler, &outbox.Message{ID: poison.ID, Seq: poison.Seq}, poison); parkErr == nil {
				maxSeq = max(maxSeq, poison.Seq)
				parkedPoison = true
				r.noteSendOK(poison.Seq)
			}
			// An unconfirmed park falls through to the terminal switch below, which
			// is where the wedge is reported — the same place the no-handler case
			// reaches, so the two share one escalation site.
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
				oldestAge = r.oldestAge(ctx, msgs[0].CreateTime)
			}
			notify.Drained(r.options.Observer, r.name, sent, oldestAge, more)
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
			// No PoisonHandler (or an unconfirmed park): what succeeded is
			// committed; surface the decode failure — joined with the park
			// failure, if any, so a broken DLQ is visible in the error chain,
			// not just in telemetry — and stop the lane at the poison row.
			//
			// This is the DEFAULT wedge: retrying an undecodable row can never
			// succeed, so with no handler configured the lane stops here forever.
			// The escalation matters most in exactly this case, and the park branch
			// above cannot report it (it is gated on PoisonHandler being set), so
			// report it here — noteStuck is idempotent per position, so a
			// double-report from the park path is not possible.
			r.noteStuck(poison.Seq, poison.ID, errors.Join(listErr, parkErr))

			return errors.Join(listErr, parkErr)
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

// noteSendOK ends a stuck episode after the message AT seq was successfully
// disposed of (sent, or parked with confirmation). Keyed on seq, not
// unconditional: a pass that re-delivers a prefix ahead of a wedged row would
// otherwise reset the escalation timer every tick — see notify.StuckTracker.
//
// This runs once per delivered message, so the seq is handed over as-is: the
// tracker compares positions directly and nothing is formatted here.
func (r *Relay) noteSendOK(seq int64) { r.stuck.Progress(seq) }

// noteStuck escalates a lane that keeps stopping at the same seq (see
// notify.StuckTracker). Must be called from EVERY path that stops the lane — a
// send failure, an unsendable message whose park was not confirmed, and a poison
// row that was not parked (whether because no handler is configured, the default,
// or because the park failed) — since each of them wedges the log identically.
//
// The position label is built only when an escalation fires (at most once per
// episode), keeping its allocation off the per-message path.
func (r *Relay) noteStuck(seq int64, id string, err error) {
	stuckFor, escalate := r.stuck.Stuck(seq)
	if !escalate {
		return
	}

	notify.StuckLane(r.options.Observer, r.options.Logger, "sequence", r.name,
		"seq "+strconv.FormatInt(seq, 10), id, stuckFor, err)
}

// oldestAge reports the age of the oldest event in a drained page — the lag
// value handed to OnDrained.
//
// createTime is stamped by the STORE (the DB's NOW at insert), so the age is
// computed against the store's clock whenever the store offers one (see Clock):
// subtracting a DB-stamped timestamp from the relay host's time.Now() folds any
// NTP skew between the two into the metric, and on a pod whose clock trails the
// database it reports a NEGATIVE age for a genuinely stale backlog — which any
// gauge wired to OnDrained plots as ~0, so the lag alert never fires. Without
// the capability (or if the clock read fails) it falls back to the host clock,
// which is correct to within the skew.
//
// The store clock is read at most once per drain pass — a pass can walk many
// pages — and only when an OnDrained observer will actually consume the value,
// so an unobserved relay issues no extra query.
func (r *Relay) oldestAge(ctx context.Context, createTime time.Time) time.Duration {
	if r.options.Observer.OnDrained == nil {
		return 0 // nobody consumes it: don't pay for a clock read
	}

	now := time.Now()
	if r.clock != nil {
		if r.passStoreNow.IsZero() {
			storeNow, err := r.clock.StoreNow(ctx)
			if err != nil {
				// Lag is telemetry: a failed clock read must not fail the pass.
				// Latch the host clock so the fallback is reported once per pass.
				r.options.Logger.Warn("sequence relay: store clock read failed; reporting lag against the host clock",
					"relay", r.name, "err", err)
				storeNow = now
			}
			r.passStoreNow = storeNow
		}
		now = r.passStoreNow
	}

	age := now.Sub(createTime)
	if age < 0 {
		// Only reachable on the host-clock fallback (a store clock cannot predate
		// its own insert). A negative lag is not a signal any gauge can use.
		return 0
	}
	return age
}

// maybeSweep runs the retention sweep at most once per RetentionSweepInterval
// of wall-clock time — decoupled from PollInterval, so retuning the tick does
// not silently change sweep cadence. The first leader tick sweeps immediately
// (lastSweep zero); each pass is bounded by RetentionSweepBatch and the pass
// loop by maxPagesPerTick, mirroring drain: a single bounded batch per
// interval would otherwise fall behind SILENTLY whenever deletable rows
// accumulate faster than batch/interval, growing the table toward a
// disk-full incident with no signal. Every pass reports its count via
// OnSwept — repeated full batches are the falling-behind alarm.
func (r *Relay) maybeSweep(ctx context.Context) error {
	if r.retention == nil {
		return nil
	}
	if !r.lastSweep.IsZero() && time.Since(r.lastSweep) < r.options.RetentionSweepInterval {
		return nil
	}
	r.lastSweep = time.Now()
	for range maxPagesPerTick {
		n, err := r.retention.SweepMessages(ctx, r.options.RetentionWindow, r.options.RetentionSweepBatch)
		if err != nil {
			return err
		}
		// Reported even for n == 0. A zero sweep is not always the healthy
		// idle case: the cutoff is MIN(last_seq) across ALL groups, so one lagging
		// or replaying group (a WithStartFromBeginning group registers at 0) pins it
		// and blocks pruning store-wide for as long as it lags. Gating the signal on
		// n > 0 made a blocked sweep and a healthy one both emit NOTHING, so the log
		// could grow toward disk-full with no way to tell the two apart. The loop
		// exits immediately on a short page, so an idle interval reports once.
		notify.Swept(r.options.Observer, r.name, n)
		if n < r.options.RetentionSweepBatch {
			return nil // drained the deletable backlog
		}
		if err := ctx.Err(); err != nil {
			return err
		}
	}
	return nil // cap hit: the next interval continues; OnSwept has been signaling full batches
}

// handleError routes a genuine per-message failure to the shared relay error
// policy. h is the configured PoisonHandler for parkable poison rows and nil
// for stop-the-lane send failures (observe+log only — the message will be
// retried, not parked). Returns the handler's park confirmation error (nil
// when h is nil). Never called for shutdown cancellation: a canceled run
// context stops the lane instead.
func (r *Relay) handleError(ctx context.Context, h relay.PoisonHandler, msg *outbox.Message, err error) error {
	return notify.MessageFailure(ctx, h, r.options.Observer, r.options.Logger, "sequence", r.name, msg, err)
}
