package sequence

import (
	"context"
	"errors"
	"strconv"
	"time"

	"github.com/quarks-tech/protoevent-go/pkg/transport/outbox"
	"github.com/quarks-tech/protoevent-go/pkg/transport/outbox/internal/lane"
)

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
	defer r.reporter.ReleaseLeadership(r.leader)

	// Do one pass immediately instead of waiting a full PollInterval for the
	// first tick, so a freshly started relay starts delivering right away.
	if ctx.Err() != nil {
		return context.Cause(ctx)
	}
	if err := r.RunOnce(ctx); err != nil {
		if ctx.Err() == nil {
			// Only report if this isn't a planned shutdown (see the same-shaped
			// handling in the loop below for the rationale).
			r.reporter.PassFailed(err)
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
					// must be reported below.
					continue
				}
				r.reporter.PassFailed(err)
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
	r.reporter.Leadership(isLeader)
	if !isLeader {
		return nil
	}

	if err := r.sequence(ctx); err != nil {
		if errors.Is(err, errLostLeadership) {
			r.reporter.Leadership(false)

			return nil // clean stop: losing leadership is not an error
		}

		return err
	}
	if err := r.drain(ctx); err != nil {
		if errors.Is(err, errLostLeadership) {
			r.reporter.Leadership(false)

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
			r.reporter.Sequenced(n)
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

// pageOutcome is why one drained page ended, and therefore whether the drain
// loop continues.
type pageOutcome int

const (
	// pageDone: the page was short — the log is drained for this tick.
	pageDone pageOutcome = iota
	// pageFull: the page was full (or a poison row was parked), so more work
	// may be waiting immediately.
	pageFull
	// pageStopped: the lane stopped on a message. Already reported.
	pageStopped
	// pageCanceled: the run context died mid-page. A planned shutdown.
	pageCanceled
)

// drain forwards messages in Seq order, committing the offset only after a
// successful send. A send failure stops the lane — the failed message retries
// next tick, preserving both order and delivery — unless an
// UnsendableClassifier calls it permanent for that message (see internal/lane,
// which owns that policy for both runtimes). Loops while pages are full,
// bounded by maxPagesPerTick so a sustained backlog cannot starve the sweep,
// renewing the leader lease between pages so a long backlog cannot outlive the
// lease — bounding stale-leader overlap to a single page.
func (r *Relay) drain(ctx context.Context) error {
	// One store-clock read per pass, not per page (see oldestAge).
	r.passStoreNow = time.Time{}

	offset, err := r.primeOffset(ctx)
	if err != nil {
		return err
	}

	for range maxPagesPerTick {
		next, outcome, err := r.drainPage(ctx, offset)
		offset = next
		if err != nil {
			return err
		}
		switch outcome {
		case pageCanceled:
			// Shutdown: successes are already committed (the final commit goes
			// through bound.Commit's detached context — the run ctx is already
			// dead here); Run's pass-level quieting keeps this silent.
			return context.Cause(ctx)
		case pageStopped, pageDone:
			return nil
		case pageFull:
		}

		// Another page follows. Renew the lease first — without this a long
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

// primeOffset returns the offset this drain pass starts from, registering the
// consumer group's offset row when it has none.
//
// Gated on the ROW's existence, not on a "primed this process" flag and not on
// offset == 0. Both alternatives are wrong in one direction each: a memory latch
// makes a deleted offset row (the documented DeleteOffset decommission, applied
// to a running group) replay the whole retained log, because the relay reads 0
// and skips the re-prime; keying on offset == 0 instead re-primes every tick for
// as long as a group legitimately sits at 0, which is three round trips per
// PollInterval — including a write, or an INSERT that fails on duplicate key and
// litters the database's error log. Existence answers the actual question.
func (r *Relay) primeOffset(ctx context.Context) (int64, error) {
	offset, exists, err := r.store.Offset(ctx, r.name)
	if err != nil || exists {
		return offset, err
	}

	if r.options.StartFromBeginning {
		// A replaying group must still REGISTER its offset row before it reads
		// anything: retention protection is derived from MIN(last_seq) across the
		// existing offset rows, so until this group has a row of its own the sweep
		// computes the cutoff from the other groups alone and can delete the very
		// history this group was configured to replay. Committing 0 is the
		// registration primitive — CommitOffset is an insert-if-absent monotone
		// upsert, so it creates the row at 0 and is a no-op against any existing row.
		return offset, r.store.CommitOffset(ctx, r.name, 0)
	}

	// A brand-new group starts at "latest" (parity with the stream runtime's
	// start-at-now) unless the caller opted into a full replay. InitOffsetLatest is
	// insert-if-absent, so re-running it against an existing row — even one
	// committed at 0 — is harmless: an existing row is never modified.
	return r.store.InitOffsetLatest(ctx, r.name)
}

// drainPage forwards one page of messages and commits what it delivered,
// returning the offset the next page starts from.
func (r *Relay) drainPage(ctx context.Context, offset int64) (int64, pageOutcome, error) {
	msgs, listErr := r.store.ListMessages(ctx, offset, r.options.BatchSize)
	poison, isPoison := errors.AsType[*DecodeError](listErr)
	if listErr != nil && !isPoison {
		return offset, pageDone, listErr
	}
	if len(msgs) == 0 && poison == nil {
		return offset, pageDone, nil
	}

	maxSeq := offset
	sent := 0
	outcome := pageDone
	for _, m := range msgs {
		// A canceled run context is a shutdown, not a message fault: stop the
		// lane before touching the next message, so a canceled ctx can't walk
		// the rest of the page fail-fast (and, with a PoisonHandler, park
		// healthy messages).
		if ctx.Err() != nil {
			outcome = pageCanceled

			break
		}
		switch r.lane.Send(ctx, m.Seq, m) {
		case lane.Sent:
			maxSeq, sent = m.Seq, sent+1
		case lane.Parked:
			maxSeq = m.Seq
		case lane.Canceled:
			outcome = pageCanceled
		case lane.Stopped:
			outcome = pageStopped
		}
		if outcome != pageDone {
			break // stop-the-lane: leave this seq for the next tick
		}
	}

	// A poison row (persisted metadata failed to decode) sits right after the
	// decoded prefix. With a PoisonHandler it is parked like any other failed
	// message and the lane advances past it; without one the lane stops at it
	// (at-least-once, order preserved). Skipped on shutdown and when the lane
	// already stopped before reaching it.
	parkedPoison := false
	var parkErr error
	if poison != nil && outcome == pageDone && r.options.PoisonHandler != nil && ctx.Err() == nil {
		stub := &outbox.Message{ID: poison.ID, Seq: poison.Seq}
		if parkErr = r.lane.Park(ctx, poison.Seq, stub, poison); parkErr == nil {
			maxSeq = max(maxSeq, poison.Seq)
			parkedPoison = true
		}
		// An unconfirmed park falls through to the terminal switch below, which
		// is where the wedge is reported — the same place the no-handler case
		// reaches, so the two share one escalation site.
	}

	if maxSeq > offset {
		if err := r.store.CommitOffset(ctx, r.name, maxSeq); err != nil {
			return offset, pageDone, err
		}
		// A single leader owns the watermark, so the value just committed IS the
		// new offset: advance locally instead of re-querying Offset for every
		// page (a wasted round trip per full page).
		offset = maxSeq
	}

	full := len(msgs) == r.options.BatchSize
	if len(msgs) > 0 || parkedPoison {
		// `sent` counts successful sends only; parked messages are reported via
		// OnError and excluded (relay.Observer contract). A poison-only page
		// (empty decoded prefix, poison parked) still disposed of a message, so
		// it is reported too — with a zero oldestAge, since there is no decoded
		// row to anchor the lag on.
		more := outcome != pageDone || full || poison != nil
		oldestAge := time.Duration(0)
		if len(msgs) > 0 {
			oldestAge = r.oldestAge(ctx, msgs[0].CreateTime)
		}
		r.reporter.Drained(sent, oldestAge, more)
	}

	switch {
	case outcome != pageDone:
		return offset, outcome, nil
	case poison != nil && !parkedPoison:
		// No PoisonHandler (or an unconfirmed park): what succeeded is committed;
		// surface the decode failure — joined with the park failure, if any, so a
		// broken DLQ is visible in the error chain, not just in telemetry — and
		// stop the lane at the poison row.
		//
		// This is the DEFAULT wedge: retrying an undecodable row can never
		// succeed, so with no handler configured the lane stops here forever. The
		// escalation matters most in exactly this case, and lane.Park cannot
		// report it (it is only reached when a handler is set), so report it here —
		// the tracker is idempotent per position, so a double-report is not
		// possible.
		err := errors.Join(listErr, parkErr)
		r.lane.Stuck(poison.Seq, poison.ID, err)

		return offset, pageDone, err
	case !full && !parkedPoison:
		return offset, pageDone, nil
	}

	return offset, pageFull, nil
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
		n, err := r.retention.SweepMessages(ctx, r.options.RetentionSweepBatch)
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
		r.reporter.Swept(n)
		if n < r.options.RetentionSweepBatch {
			return nil // drained the deletable backlog
		}
		if err := ctx.Err(); err != nil {
			return err
		}
	}

	return nil // cap hit: the next interval continues; OnSwept has been signaling full batches
}

// stuckLabel renders a seq for the operator. Built only on escalation (see
// lane.Lane.Label), so its allocation stays off the per-message path.
func stuckLabel(seq int64, _ string) string {
	return "seq " + strconv.FormatInt(seq, 10)
}
