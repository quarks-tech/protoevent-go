package sequence

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/quarks-tech/protoevent-go/pkg/transport/outbox"
	"github.com/quarks-tech/protoevent-go/pkg/transport/outbox/internal/bound"
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

// errLeaseRenewFailed additionally marks a renewal that could not be EVALUATED —
// the leader store itself errored — as opposed to one that cleanly reported the
// lock lost. It always accompanies errLostLeadership, because the two demand the
// same caution: a renewal whose answer we never got is not a lease we may claim
// to hold, so the pass stops and the sweep is skipped either way.
//
// They are told apart only in what RunOnce RETURNS. A clean loss is nil — losing
// an election is not a fault. A failed renewal is the store error itself, so the
// tick is reported through PassFailed instead of vanishing: swallowing it left a
// leader store that errored on every renewal indistinguishable from a relay that
// simply is not the leader, which is silence in front of a real outage.
var errLeaseRenewFailed = errors.New("sequence: leader lease renewal failed")

// commitOffset advances the watermark, through the fence when the store has one.
//
// A rejected fenced write is reported as errLostLeadership, which is what it means: a
// changed holder_id says another instance has taken the lock, so this pass must stop
// and skip the sweep exactly as a failed renewal does. It is also SIGNALED rather
// than swallowed — the stream runtime surfaces a stale token save to the observer, and
// the same event here was previously invisible, which is why a superseded leader could
// rewind a group with nothing in the logs to say so.
func (r *Relay) commitOffset(ctx context.Context, seq int64) error {
	if r.fenced == nil {
		return r.store.CommitOffset(ctx, r.name, seq)
	}

	persisted, err := r.fenced.CommitOffsetFenced(ctx, r.name, r.leader.LockName(), r.leader.HolderID(), seq)
	if err != nil {
		return err
	}
	if !persisted {
		r.reporter.Error(fmt.Errorf("%w: the watermark commit at seq %d was rejected because the "+
			"leader lock %q is now held by another instance; this relay was superseded mid-pass and its "+
			"offset was NOT applied", errLostLeadership, seq, r.leader.LockName()))

		return errLostLeadership
	}

	return nil
}

// renewLease re-acquires the leader lease between pages. See the two sentinels
// above for how its three outcomes are reported.
func (r *Relay) renewLease(ctx context.Context) error {
	isLeader, err := r.leader.TryAcquire(ctx)
	switch {
	case err != nil:
		return fmt.Errorf("%w: %w: %w", errLostLeadership, errLeaseRenewFailed, err)
	case !isLeader:
		return errLostLeadership
	}

	return nil
}

// Run drives the relay until ctx is canceled, then releases leadership so a
// planned shutdown fails over in well under LeaseTTL.
func (r *Relay) Run(ctx context.Context) error {
	ticker := time.NewTicker(r.options.PollInterval)
	defer ticker.Stop()
	// One budget for the shutdown tail; see bound.ShutdownScope.
	shutdown, endShutdown := bound.ShutdownScope()
	defer endShutdown()
	defer r.reporter.ReleaseLeadership(shutdown, r.leader)

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
			// Cancellation is re-checked BEFORE starting a pass, because a ready
			// ticker and a done context are both ready cases and select picks
			// pseudo-randomly — so this arm keeps being chosen after cancellation.
			// Each pass entered that way starts a fresh store call, and the final
			// commit's shutdown grace is paid again for every one of them: measured at
			// three passes and 15s of shutdown where one pass and 5s were intended.
			// (The receive loop in transport/gochan documents the same hazard.)
			if ctx.Err() != nil {
				return context.Cause(ctx)
			}
			if err := r.RunOnce(ctx); err != nil {
				if ctx.Err() != nil {
					// Planned shutdown (ctx is genuinely done): don't report a
					// spurious error metric/log for it. Gated on run-context
					// liveness, not error identity — an op-level
					// context.DeadlineExceeded while ctx is still alive is a real,
					// recurring timeout and must be reported below.
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
//
// The sweep runs even when sequence or drain FAILED, and its error is joined
// rather than replacing theirs. Returning early on a delivery error — which is
// what this used to do — coupled retention to delivery in the one case where
// they must be independent: a single undecodable row makes drainPage return an
// error every tick under the default no-PoisonHandler config, so the sweep was
// never reached again and the log grew unbounded toward disk-full. Worse, the
// documented alarm for exactly that ("OnSwept keeps reporting 0 while the table
// keeps growing") could not fire either, because OnSwept stopped being called
// rather than reporting 0. Sweeping is safe here regardless: the cutoff is
// MIN(last_seq) across committed offsets, so a stalled lane simply pins it and
// the sweep reports 0 — visibly.
//
// Two cases still skip it, both because sweeping would be wrong rather than
// merely unhelpful: a lost lease (a known non-leader must not sweep) and a dead
// run context (a shutdown, where the sweep would only latch its own interval
// clock forward without doing any work).
func (r *Relay) RunOnce(ctx context.Context) error {
	isLeader, err := r.leader.TryAcquire(ctx)
	if err != nil {
		return err
	}
	r.reporter.Leadership(isLeader)
	if !isLeader {
		return nil
	}

	// Prime BEFORE sequencing. InitOffsetLatest registers a new group at the
	// current maximum ASSIGNED seq, so priming after a sequencer pass registers it
	// at the top of the backlog that pass had just labeled — permanently
	// discarding every committed, undelivered row, silently. Priming first is what
	// makes InitOffsetLatest's own contract true: "a group primed before the
	// sequencer catches up will receive that backlog once it is sequenced."
	offset, primeErr := r.primeOffset(ctx)

	seqErr := r.sequence(ctx)
	// A sequencer fault does not implicate the drain, so it must not skip it: the
	// drain reads rows that ALREADY carry a seq and advances a monotone offset,
	// with no data dependency on this tick's sequencer pass. Skipping it means a
	// store-side fault with nothing to do with delivery halts delivery of the
	// whole readable backlog — and the trigger is the DEFAULT multi-group
	// configuration, where every group runs the sequencer and contends on the one
	// pessimistic counter-row lock, so the loser of each contended pass never
	// drains. (This is the same decoupling maybeSweep already gets below, applied
	// where the blast radius is delivery itself.)
	//
	// The one exception is a lost lease, where the drain has no authority to run
	// at all. A dead run context is NOT an exception: drain is what observes the
	// cancellation and reports it as context.Cause, so skipping it here would
	// return a nil error from a pass that never ran.
	//
	// A failed prime skips only the drain, which cannot run without a starting
	// offset; the sequencer pass above is unaffected for the same reason the drain
	// is unaffected by a sequencer fault.
	var drainErr error
	if primeErr == nil && !errors.Is(seqErr, errLostLeadership) {
		drainErr = r.drain(ctx, offset)
	}

	// Joined in phase order, one named error each. errors.Is over a join is
	// order-independent, so the leadership triage below reads the same either way —
	// but a single rebound variable made the gate above depend on evaluation order.
	passErr := errors.Join(primeErr, seqErr, drainErr)
	if errors.Is(passErr, errLostLeadership) {
		r.reporter.Leadership(false)
		if errors.Is(passErr, errLeaseRenewFailed) {
			// Not an election we lost — one we could not run. Report it.
			return passErr
		}

		return nil // clean stop: losing leadership is not an error
	}
	if ctx.Err() != nil {
		return passErr
	}

	return errors.Join(passErr, r.maybeSweep(ctx))
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
		// draining, or sweeping. RunOnce maps a clean loss to a nil stop and a
		// failed renewal to a reported error (see renewLease).
		if err := r.renewLease(ctx); err != nil {
			return err
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
func (r *Relay) drain(ctx context.Context, offset int64) error {
	// One store-clock read per pass, and one skew warning per pass, not per page
	// (see oldestAge).
	r.passStoreNow, r.passSkewWarned = time.Time{}, false

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
		// skips the sweep instead of running it as a known non-leader (see
		// renewLease for how the two outcomes are reported).
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := r.renewLease(ctx); err != nil {
			return err
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
		return offset, r.commitOffset(ctx, 0)
	}

	// A brand-new group starts at "latest" (parity with the stream runtime's
	// start-at-now) unless the caller opted into a full replay. InitOffsetLatest is
	// insert-if-absent, so re-running it against an existing row — even one
	// committed at 0 — is harmless: an existing row is never modified.
	return r.store.InitOffsetLatest(ctx, r.name)
}

// forward delivers one page's decoded messages and reports how far the page got:
// the highest seq the caller may commit, how many were actually sent (what
// OnDrained counts), and why it stopped.
//
// Two shapes, one policy. Both drive internal/lane, which owns every decision about
// what a failure means; the difference is only how many acknowledgements are in
// flight while it does. The batched shape runs when the transport implements
// eventbus.BatchSender, and it is what takes the relay off the one-round-trip-per-
// event ceiling — serially the drain rate is the reciprocal of the confirm latency,
// which measured 918 events/s against a loopback quorum queue and extrapolates to
// 129/s at a 5ms cross-AZ confirm.
func (r *Relay) forward(ctx context.Context, msgs []*outbox.Message, offset int64) (maxSeq int64, sent int, outcome pageOutcome) {
	if r.lane.Batching() {
		return r.forwardBatched(ctx, msgs, offset)
	}

	maxSeq, outcome = offset, pageDone
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

	return maxSeq, sent, outcome
}

// forwardBatched is forward's overlapped-acknowledgement shape.
//
// It LOOPS rather than making one call, because a parked message is a resumption
// point: the lane advances past a message it could park and the rest of the page is
// still deliverable. Each iteration hands the lane the remaining suffix, so a page
// with three parkable messages costs four batches instead of degrading to one
// message at a time.
//
// Committing stays exactly as strict as the serial path: maxSeq only ever moves to
// the last position the lane said may be advanced past, which is its CONTIGUOUS
// confirmed prefix (plus a confirmed park). A transport that delivered messages
// beyond a failure does not move it — those are re-delivered next pass, which is
// the at-least-once trade the relay already makes everywhere else.
func (r *Relay) forwardBatched(ctx context.Context, msgs []*outbox.Message, offset int64) (int64, int, pageOutcome) {
	// Positions parallel to msgs, in the buffer reused across pages.
	r.seqs = r.seqs[:0]
	for _, m := range msgs {
		r.seqs = append(r.seqs, m.Seq)
	}

	maxSeq, sent := offset, 0
	for i := 0; i < len(msgs); {
		// Same shutdown pre-check as the serial loop, for the same reason.
		if ctx.Err() != nil {
			return maxSeq, sent, pageCanceled
		}

		n, advanced, d := r.lane.SendBatch(ctx, r.seqs[i:], msgs[i:])
		sent += n
		if advanced > 0 {
			maxSeq = msgs[i+advanced-1].Seq
		}
		i += advanced

		switch d {
		case lane.Sent:
			// The whole remainder went; i is now len(msgs) and the loop ends.
		case lane.Parked:
			// Advanced past the parked message: continue with what is left.
		case lane.Canceled:
			return maxSeq, sent, pageCanceled
		case lane.Stopped:
			return maxSeq, sent, pageStopped
		}
	}

	return maxSeq, sent, pageDone
}

// drainPage forwards one page of messages and commits what it delivered,
// returning the offset the next page starts from.
func (r *Relay) drainPage(ctx context.Context, offset int64) (int64, pageOutcome, error) {
	msgs, listErr := r.store.ListMessages(ctx, offset, r.options.BatchSize)
	poison, isPoison := errors.AsType[*DecodeError](listErr)
	if listErr != nil && !isPoison {
		// A page the store cannot read wedges the lane at this offset exactly as a
		// send failure does, and unlike a poison row no PoisonHandler can clear it:
		// parking is reachable only from the poison branch below. Without this call
		// the 15-minute escalation was structurally unreachable for the worst class
		// of wedge — a create_time that will not scan (a DSN missing
		// parseTime=true), an event_id that is not a UUID — leaving a relay that
		// delivers nothing for the group's lifetime looking like a two-second blip.
		//
		// A dead run context is a shutdown, not a wedge, and must not escalate.
		if ctx.Err() == nil {
			r.lane.Stuck(offset, "", listErr)
		}

		return offset, pageDone, listErr
	}
	if len(msgs) == 0 && poison == nil {
		return offset, pageDone, nil
	}

	maxSeq, sent, outcome := r.forward(ctx, msgs, offset)

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
		if err := r.commitOffset(ctx, maxSeq); err != nil {
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
// createTime is stamped by the STORE at insert, so the age is computed against the
// store's own clock whenever the store offers one (see Clock): subtracting that
// timestamp from an unrelated clock's time.Now() folds the skew between the two
// into the metric. Without the capability (or if the clock read fails) it falls
// back to the host clock, which is correct to within that skew.
//
// The store clock is read at most once per drain pass — a pass can walk many
// pages — and only when an OnDrained observer will actually consume the value,
// so an unobserved relay issues no extra query.
//
// A NEGATIVE age is reported as negative, not clamped to zero, and that is a
// deliberate reversal. Clamping was justified by "a store clock cannot predate its
// own insert", which is not true of the reference store: the tidb store stamps
// create_time from the PUBLISHER process's clock and answers StoreNow from the
// RELAY process's clock (see Clock), so a relay pod whose clock trails the
// publishers produces a negative age for a genuinely stale backlog. Clamping that
// to 0 renders it as a perfectly healthy relay on any gauge wired to OnDrained —
// which is the precise failure mode the Clock capability was added to eliminate,
// reappearing one layer down and invisible. A negative value is not a lag an alert
// can threshold either, but it is unmistakably wrong on a dashboard, and the log
// line below names the cause. Hiding it was the worse of the two.
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
	if age < 0 && !r.passSkewWarned {
		// Once per pass, for the same reason the failed-clock-read warning above is:
		// a multi-page pass would otherwise log this per page.
		r.passSkewWarned = true
		r.options.Logger.Warn("sequence relay: negative event age — the clock that stamped the row "+
			"is ahead of the clock the lag is measured against; reported lag is skewed by at least "+
			"this much and a real backlog may read as healthy",
			"relay", r.name, "age", age, "create_time", createTime, "measured_against", now)
	}

	return age
}

// maybeSweep runs the retention sweep, at most once per RetentionSweepInterval of
// wall-clock time ONCE IT HAS CAUGHT UP — decoupled from PollInterval, so retuning the
// tick does not silently change the idle cadence. The first leader tick sweeps
// immediately (lastSweep zero); each pass is bounded by RetentionSweepBatch and the pass
// loop by maxPagesPerTick, mirroring drain.
//
// While there is deletable work LEFT the interval is not consumed, so the sweep resumes
// on the next tick. That distinction is the difference between a bounded and an unbounded
// table: latching the interval on any completed page capped the sweep at
// maxPagesPerTick * RetentionSweepBatch per interval — 17.8 rows/s on the defaults —
// below any real publish rate, so a store publishing faster grew at the difference
// forever and surfaced as a disk-full incident months later, nowhere near the cause.
//
// Every pass reports its count via OnSwept — repeated full batches mean the sweep is
// behind and now working every tick to catch up.
func (r *Relay) maybeSweep(ctx context.Context) error {
	if r.retention == nil {
		return nil
	}
	if !r.lastSweep.IsZero() && time.Since(r.lastSweep) < r.options.RetentionSweepInterval {
		return nil
	}
	for range maxPagesPerTick {
		n, err := r.retention.SweepMessages(ctx, r.options.RetentionSweepBatch)
		if err != nil {
			// Deliberately WITHOUT latching lastSweep: a sweep that deleted nothing
			// must not buy a full RetentionSweepInterval of silence. Latching before
			// the call meant any transient fault — a lock wait, a query-memory
			// cancellation, a connection blip — deferred the next attempt by an hour
			// on the default cadence while the table kept growing, and turned the
			// retry cadence for a momentary fault into an hourly one.
			//
			// The interval bounds how often a SUCCESSFUL sweep runs; it is not a
			// ration of attempts. A permanently failing sweep therefore retries once
			// per tick, which is the same cadence every other store fault gets and is
			// reported through PassFailed just like them.
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
			// Latched only HERE, on catching up: the interval is a cadence for pruning
			// an outbox with nothing left to delete, not a ration of throughput. It
			// used to be latched on the first completed page, so exiting the loop at
			// maxPagesPerTick with pages still full bought a full interval of silence
			// and capped the sweep at maxPagesPerTick*RetentionSweepBatch per interval
			// — 17.8 rows/s on the defaults, below any real publish rate, so the table
			// grew at (rate - 17.8) rows/s until the disk filled months later.
			//
			// This is the same reasoning the error path above already applies, reached
			// by a different exit: while there is known deletable work left, the sweep
			// resumes next tick, bounded per tick by maxPagesPerTick exactly as before.
			r.lastSweep = time.Now()

			return nil // drained the deletable backlog
		}
		if err := ctx.Err(); err != nil {
			return err
		}
	}

	// Cap hit with pages still full: deliberately NOT latching lastSweep, so the next
	// tick continues instead of waiting out the interval. OnSwept has been signaling
	// full batches throughout, which is the falling-behind alarm.
	return nil
}

// stuckLabel renders a seq for the operator. Built only on escalation (see
// lane.Lane.Label), so its allocation stays off the per-message path.
func stuckLabel(seq int64, _ string) string {
	return "seq " + strconv.FormatInt(seq, 10)
}
