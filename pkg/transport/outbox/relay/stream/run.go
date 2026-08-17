package stream

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/quarks-tech/protoevent-go/pkg/transport/outbox"
	"github.com/quarks-tech/protoevent-go/pkg/transport/outbox/internal/bound"
	"github.com/quarks-tech/protoevent-go/pkg/transport/outbox/internal/lane"
)

// ErrLaneStopped is returned by RunOnce when drainWindow stopped the lane (a
// send failure, or a poison event without a parking path) and closed the
// stream. It is not a failure of the pass: the caller should back off and
// call RunOnce again — the reopen resumes from the last persisted token and
// redelivers the stopped event. Run handles this itself; the sentinel is
// exported for callers driving RunOnce directly, who otherwise could not
// distinguish "back off and retry" from a transient store error.
var ErrLaneStopped = errors.New("stream: lane stopped; will reopen and redeliver")

// Run drives the relay until ctx is canceled (returns ctx.Err()) or a fatal
// stream condition occurs (returns ErrInvalidated / ErrHistoryLost).
// Releases leadership on exit so a planned shutdown fails over quickly.
func (r *Relay) Run(ctx context.Context) error {
	defer r.closeStream()
	defer r.reporter.ReleaseLeadership(r.leader)

	r.ensureIndexes(ctx)

	for {
		if ctx.Err() != nil {
			// context.Cause degrades to ctx.Err() under a plain WithCancel and
			// surfaces the richer cancel reason under WithCancelCause/errgroup.
			return context.Cause(ctx)
		}
		err := r.RunOnce(ctx)
		switch {
		case err == nil:
			// keep looping; RunOnce already blocked ~one drain window
		case errors.Is(err, ErrLaneStopped):
			// Stream already closed by drainWindow; back off, then reopen and
			// redeliver next iteration. A send failure was already observed
			// by the lane, so don't re-observe here.
			r.sleep(ctx)
		case errors.Is(err, ErrInvalidated), errors.Is(err, ErrHistoryLost):
			// Fatal: report and stop. The deferred closeStream releases the cursor.
			r.reporter.Error(err)
			r.options.Logger.Error("stream relay: fatal stream error", "relay", r.name, "err", err)

			return err
		case ctx.Err() != nil:
			// Planned shutdown (ctx is genuinely done): the ctx.Err() check at
			// the top of the loop will return next iteration; don't report a
			// spurious error metric/log. Gated on run-context liveness, not
			// error identity — the mongo v2 driver's own operation timeouts
			// also surface as context.DeadlineExceeded, and those must still
			// be observed as real errors while ctx is alive (default branch).
			r.closeStream()
			r.sleep(ctx)
		default:
			// transient (leadership, reopen, send/save): report, drop the stream, retry
			r.reporter.PassFailed(err)
			r.closeStream()
			r.sleep(ctx)
		}
	}
}

// ensureIndexes creates the store's retention indexes once at start, when the
// store offers that capability (IndexEnsurer). Idempotent by contract, so every
// replica may call it.
//
// A failure is logged, never returned. The two ways it fails are both survivable
// and neither is a reason to refuse to deliver events: the relay's credentials
// may not permit DDL because indexes are managed by migrations, and a retention
// value changed against an existing collection is rejected outright
// (IndexOptionsConflict — MongoDB requires collMod, see the store's own hint).
// Failing Run on either would turn a retention misconfiguration into a total
// delivery outage, which is strictly worse than the unbounded growth it warns
// about.
func (r *Relay) ensureIndexes(ctx context.Context) {
	if r.ensurer == nil {
		return
	}
	if err := r.ensurer.EnsureIndexes(ctx); err != nil {
		r.options.Logger.Warn("stream relay: could not ensure the store's retention indexes; "+
			"the outbox collection may grow without bound until this is resolved",
			"relay", r.name, "err", err)
	}
}

// RunOnce performs one leader-gated drain window. Non-leaders return nil without
// touching the stream. Exposed for tests; Run calls it in a loop.
//
// Every store call here is bounded (boundedStore, bounded.go; TryAcquire
// self-bounds): a wedged network call must surface as an error instead of
// silently stalling the relay past its own lease.
func (r *Relay) RunOnce(ctx context.Context) error {
	isLeader, err := r.leader.TryAcquire(ctx)
	if err != nil {
		return err
	}
	r.reporter.Leadership(isLeader)
	if !isLeader {
		r.closeStream()
		r.sleep(ctx) // avoid busy-spinning TryAcquireLeaderLock while not leader
		return nil
	}
	if r.stream == nil {
		stored, storedCT, err := r.store.LoadToken(ctx, r.name)
		if err != nil {
			return err
		}
		// Kept apart from what we RESUME from, so neither variable has to mean two
		// things: only a store-sourced token counts as "already persisted" below.
		token, ct := stored, storedCT
		if token == "" && r.baselineTok != "" {
			// The store still has no row for this group, but this process already
			// established a baseline that simply failed to persist. Resuming from it
			// is what keeps the reopen from becoming a second, later "now" with the
			// events in between dropped.
			token, ct = r.baselineTok, r.baselineCT
		}
		s, err := r.store.Watch(ctx, token, r.options.DrainWindow)
		if err != nil {
			return err
		}
		r.stream = s
		r.committedCT = ct
		// Only a token that came from the STORE may seed lastSavedTok: it is the
		// "already persisted, skip the no-op save" marker, and seeding it with a
		// baseline that failed to persist would suppress the very save that
		// durably records the position.
		r.lastSavedTok = stored
		if token == "" {
			// Fresh consumer group: establish a resume baseline from the stream's
			// initial resume token BEFORE draining, so a first-window send failure
			// (stop-the-lane, processed==0, which persists nothing) reopens from a
			// point preceding the failed event instead of restarting at a fresh
			// "now" and silently skipping it.
			btok, bct, serverTime := r.stream.Checkpoint()
			if btok == "" {
				// Nothing resumable to anchor to: the driver has cached no token for
				// this cursor yet. The window between here and the first persisted
				// position is then unprotected, so say so rather than leaving the
				// only silent path in this function.
				r.options.Logger.Warn("stream relay: fresh consumer group has no resume baseline; "+
					"a failure before the first persisted position will restart at a later "+
					"\"now\" and skip the events in between",
					"relay", r.name)
			}
			if btok != "" {
				// Remembered BEFORE the save, because the save is exactly what may
				// fail: a baseline kept only on success is no baseline at all.
				r.baselineTok, r.baselineCT = btok, bct
				if err := r.saveToken(ctx, btok, bct); err != nil {
					return err
				}
				// A fresh group is by definition at the head, so this baseline is
				// also the first honest calibration reading.
				if serverTime {
					r.calibrateClock(bct)
				}
			}
		}
	}
	return r.drainWindow(ctx)
}

// drainWindow processes up to TokenBatchSize events (fast while buffered; one
// DrainWindow wait when idle), persists the token, and reports lag.
func (r *Relay) drainWindow(ctx context.Context) error {
	sent := 0    // successfully delivered (the ObserveDrained count)
	handled := 0 // events disposed of this window: sent or parked
	stopped := false
	advanced := false // resume position moved this window
	caughtUp := false // the window ran out of events: the relay is at the head
	var lastTok string
	var lastCT time.Time

	for range r.options.TokenBatchSize {
		if ctx.Err() != nil {
			// Planned shutdown: the driver keeps serving client-buffered events
			// without touching ctx, so without this check the loop would keep
			// pulling them, fail each Send with context.Canceled, and park
			// healthy messages. Stop the lane instead; they redeliver on restart.
			stopped = true
			break
		}
		e, ok, err := r.stream.Next(ctx)
		if err != nil {
			de, isPoison := errors.AsType[*DecodeError](err)
			if isPoison && de.ResumeToken != "" && r.options.PoisonHandler != nil && ctx.Err() == nil {
				// lane.Park owns the confirmed-park rule (advance only on nil).
				// The GATE above it is this runtime's own, and it turns on the
				// resume TOKEN alone: without one there is no position to advance
				// to, and saving "" would erase the group's stored position
				// outright, so stopping the lane really is the lesser harm.
				//
				// A missing ID is NOT part of the gate, though it used to be. Both
				// extractions are best-effort and independent (watch.go pulls the
				// token from _id and the row id from fullDocument._id), so a
				// corrupted fullDocument yields a perfectly good token with an
				// empty id — and refusing to park that left the stream reopening
				// onto the same event every window forever, with a PoisonHandler
				// installed and willing to take it. A wedge is not the lesser harm
				// there; an idless stub is. The handler gets a stub it cannot look
				// up by id, but it also gets the DecodeError, whose resume token
				// positions the event in the oplog — and the sequence runtime, which
				// gates on the handler alone, has always behaved this way.
				parkErr := r.lane.Park(ctx, stuckKey(de.ResumeToken, de.ID), &outbox.Message{ID: de.ID}, err)
				if parkErr == nil {
					// Poison event parked: resume past it. CommitTime
					// extraction is best-effort too, so lastCT FLOORS and never
					// rewinds — a zero or older commitTime would either be
					// swallowed by the store's monotone guard (freezing the
					// position at the pre-poison token, so the lane reopens onto
					// the poison every window) or persist a stale anchor and
					// inflate the cliff metric. With nothing advanced yet, anchor
					// on the last committed commitTime: equal still passes the
					// monotone guard, so the token advances.
					lastTok = de.ResumeToken
					switch {
					case de.CommitTime.After(lastCT):
						lastCT = de.CommitTime
					case lastCT.IsZero():
						lastCT = r.committedCT
					}
					advanced = true
					handled++

					continue
				}
				// Unconfirmed park: fall through below and stop the lane at
				// the poison — same as having no handler or no token — and
				// retry the park on reopen. Join the park failure into the
				// error chain: telemetry alone (lane.Park already observed and
				// logged it) would leave Run's returned error blind to the
				// broken DLQ.
				err = errors.Join(err, parkErr)
			}
			// A poison event the lane cannot get past wedges the stream: the reopen
			// lands on the same event every window, forever, and nothing behind it
			// is delivered. Escalate for EVERY such case, not just the failed-park
			// one — no PoisonHandler configured (the default) and an unextractable
			// resume token both reach here with the branch above skipped entirely,
			// and those are the likeliest wedges in the system. Non-poison Next
			// errors are stream-level, not stuck at a position, so they are left to
			// Run's reopen/backoff.
			if isPoison {
				r.lane.Stuck(stuckKey(de.ResumeToken, de.ID), de.ID, err)
			}
			// Non-parkable Next error: persist the sent prefix's position
			// first (the same save the send-failure stop path gets via
			// `case advanced:` below), so Run's close/reopen/backoff resumes
			// after the last success — at the poison — instead of re-sending
			// the prefix every DrainWindow.
			if advanced {
				if saveErr := r.saveToken(ctx, lastTok, lastCT); saveErr != nil {
					// Join rather than replace: returning only saveErr would
					// launder a fatal Next error (ErrInvalidated /
					// ErrHistoryLost) into a transient one for this pass, and
					// Run would do a spurious close/backoff/reopen before the
					// fatal resurfaced.
					return errors.Join(err, saveErr)
				}
			}
			return err
		}
		if !ok {
			// Window elapsed with no more events: the relay is at the stream head.
			caughtUp = true

			break
		}
		if e == nil || e.Message == nil {
			// Contract violation by the Store's Stream implementation (ok
			// promises a decoded event): fail loudly as a stream error rather
			// than panic the relay goroutine.
			return errors.New("stream: Next returned ok with a nil event or message")
		}
		// The shared per-message policy (internal/lane) decides send/park/stop;
		// what is left here is this runtime's own bookkeeping — which resume
		// position a disposed event advances to. A stopped lane redelivers the
		// event on reopen, so order and delivery are preserved either way.
		switch r.lane.Send(ctx, stuckKey(e.ResumeToken, e.Message.ID), e.Message) {
		case lane.Sent:
			sent++
			lastTok, lastCT = e.ResumeToken, e.CommitTime
			advanced = true
			handled++
		case lane.Parked:
			lastTok, lastCT = e.ResumeToken, e.CommitTime
			advanced = true
			handled++
		case lane.Stopped, lane.Canceled:
			// Canceled is a planned shutdown rather than an event fault, but
			// this runtime treats both the same way: stop the window without
			// advancing, and let the reopen redeliver from the persisted token.
			stopped = true
		}
		if stopped {
			break
		}
	}

	// Calibrate the clock offset on EVERY caught-up window, not only the
	// event-free ones. caughtUp means Next ran out of events, so the Checkpoint is
	// the stream head — the server's "now" — whether or not the window also
	// delivered something first. Gating this on the empty-window branch below left
	// a relay under continuous load (one insert per DrainWindow is enough) never
	// calibrating at all, so committedTokenAge silently degraded to the raw host
	// clock for the process lifetime, which is the skew-blindness the offset exists
	// to remove.
	var cpTok string
	var cpCT time.Time
	if caughtUp && !stopped {
		var serverTime bool
		cpTok, cpCT, serverTime = r.stream.Checkpoint()
		// serverTime only: a local-clock substitute would mark the offset
		// "calibrated" while it is really the raw host clock.
		if serverTime {
			r.calibrateClock(cpCT)
		}
	}

	switch {
	case advanced:
		if err := r.saveToken(ctx, lastTok, lastCT); err != nil {
			return err
		}
	case !stopped:
		// Empty window: persist Checkpoint so a caught-up-connected consumer stays
		// resumable and the lag anchor stays fresh. Skip the write
		// when the token is unchanged since the last save (an idle stream would
		// otherwise persist the same position every DrainWindow) — the lag
		// bookkeeping still advances locally.
		//
		// Reuses the reading taken above rather than calling Checkpoint twice: this
		// branch implies caughtUp, since every loop iteration either advances,
		// stops, or returns — so reaching here with neither advanced nor stopped
		// means the loop exited through the out-of-events break.
		tok, ct := cpTok, cpCT
		if tok == "" {
			break
		}
		if tok == r.lastSavedTok {
			r.committedCT = ct
			break
		}
		if err := r.saveToken(ctx, tok, ct); err != nil {
			return err
		}
	}

	more := stopped || handled == r.options.TokenBatchSize
	r.reporter.Drained(sent, r.committedTokenAge(), more)

	if stopped {
		// A change-stream cursor cannot rewind, so to actually redeliver the
		// failed event we must reopen from the last-persisted token. Close the
		// stream (next RunOnce reopens via LoadToken+Watch) and signal Run to
		// back off. An advanced window persisted the last success above (on
		// shutdown, through saveToken's fresh bounded context — the run ctx is
		// already dead there), so the reopen resumes just after it, at the
		// failed event; a non-advanced one saved nothing (reopen resumes from
		// the prior token).
		r.closeStream()
		return ErrLaneStopped
	}
	return nil
}

// saveToken persists the position and updates the local trackers used for
// no-op-save skipping and lag reporting — but ONLY when the store confirms it
// persisted: a save the monotone guard classified as stale (persisted=false,
// e.g. a stale leader finishing a slow window) must not advance
// lastSavedTok/committedCT, or committedTokenAge() — the resume-token-cliff
// early warning — would report freshness the stored row doesn't have.
// Context bounds (incl. the detached-context final save on shutdown) are
// boundedStore's job — see bounded.go.
func (r *Relay) saveToken(ctx context.Context, tok string, ct time.Time) error {
	persisted, err := r.store.SaveToken(ctx, r.name, tok, ct)
	if err != nil {
		return err
	}
	if !persisted {
		// Not an error — the store's monotone guard did its job — but not a
		// non-event either: this relay believed it was the leader and delivered a
		// window whose position another writer had already moved past, so those
		// events were very likely delivered twice. Silence made a split-brain
		// leader look identical to a healthy one, since every other signal
		// (OnDrained, leadership) reports success.
		//
		// Reported to BOTH the log and the Observer. Log-only was itself a
		// telemetry hole: WithLogger explicitly permits a discarding logger, and a
		// deployment that alarms on metrics could not see the one condition that
		// says two leaders are draining the same stream. It is NOT returned as a
		// pass error, because failing the pass would close and reopen the stream
		// over a condition the next tick's TryAcquire resolves anyway.
		r.options.Logger.Warn("stream relay: resume token rejected as stale; another writer holds a newer position",
			"relay", r.name)
		r.reporter.Error(fmt.Errorf("stream: resume token for group %q rejected as stale; another "+
			"writer holds a newer position, so this window was very likely delivered twice "+
			"(two leaders)", r.name))

		return nil
	}

	r.lastSavedTok = tok
	r.committedCT = ct
	// The store now holds a position, so the in-memory fresh-group baseline has done
	// its job and must stop being consulted. Keeping it would resurrect it whenever
	// LoadToken later returns "" — which is exactly what the documented break-glass
	// DeleteToken does, and what decommissioning a group does: the relay would reopen
	// from a position of the previous incarnation instead of at "now", and if that
	// position is the one that fell off the oplog, the recovery re-enters
	// ErrHistoryLost forever.
	r.baselineTok, r.baselineCT = "", time.Time{}

	return nil
}

// stuckKey is the stuck-lane tracking key for one event: an existing string, never
// a newly built one, because it is computed once per delivered event.
//
// The resume token identifies the position, but a poison event's token extraction
// is best-effort and can come back empty — and an empty token is exactly the case
// that wedges the lane hardest, since it is non-parkable. Fall back to the event
// id, which is extracted independently.
//
// When BOTH come back empty the key is "" — nothing about the event can be
// observed. The tracker is told so (see identified below), which is what keeps a
// later unidentifiable wedge from being mistaken for the same unresolved one.
func stuckKey(resumeToken, id string) string {
	switch {
	case resumeToken != "":
		return resumeToken
	case id != "":
		return id
	default:
		return ""
	}
}

// identified reports whether a stuck key says anything about the event at all.
// See notify.StuckTracker.Stuck.
func identified(key string) bool { return key != "" }

// stuckLabel renders a key for the operator. Built only on escalation (see
// lane.Lane.Label), so its allocation stays off the per-event path.
//
// The key is the resume token when there was one and the id otherwise, so the two
// are told apart by comparing against the id. A resume token that happened to
// equal the event id would be labeled as the id; tokens are opaque server-side
// blobs, and the consequence is a wording difference in one escalation log line.
func stuckLabel(key, id string) string {
	switch key {
	case "":
		return "unidentifiable event"
	case id:
		return "event " + id
	default:
		return "resume token " + key
	}
}

// calibrateClock records the offset between the MongoDB server clock and this
// host's, from a clusterTime observed while the relay is CAUGHT UP.
//
// Caught-up is the whole point: only then is the observed clusterTime the stream
// HEAD, i.e. the server's idea of "now". While a backlog is being replayed the
// clusterTimes coming out of the cursor are the old events' own timestamps, and
// mongo.ChangeStream.ResumeToken() likewise reports the last returned document's
// _id — so no reading taken mid-backlog says anything about the present.
func (r *Relay) calibrateClock(head time.Time) {
	if head.IsZero() || !head.After(r.calibratedHead) {
		// A head that has not advanced carries no new information about the
		// server's clock, and re-deriving the offset from it would drag the
		// estimate backwards by however long the stream has sat at that position —
		// making lag appear to stop growing. Keep the previous offset; the host
		// clock then supplies the elapsed time on its own.
		return
	}

	r.calibratedHead = head
	r.clockOffset = time.Until(head)
	r.clockCalibrated = true
}

// serverNow estimates the MongoDB server's current time, and reports whether the
// estimate is calibrated.
func (r *Relay) serverNow() (time.Time, bool) {
	if !r.clockCalibrated {
		return time.Now(), false
	}

	return time.Now().Add(r.clockOffset), true
}

// committedTokenAge is the cliff proxy: how far the committed token trails the
// PRESENT — the resume-token-cliff early-warning signal. Cheap — no query.
//
// The anchor is the estimated server clock (serverNow), never anything read from
// the cursor: a relay six hours behind reads six-hour-old events, so a
// cursor-derived anchor equals the committed position and the gauge reads ~0 for
// the whole incident — silent in exactly the case the alarm exists for. Before the
// first calibration it degrades to the raw host clock, right to within the skew.
//
// Because a commitTime is truncated to whole seconds, sub-second lag reads as 0:
// this is a cliff warning measured in minutes, not a latency histogram.
func (r *Relay) committedTokenAge() time.Duration {
	if r.committedCT.IsZero() {
		return 0
	}

	now, _ := r.serverNow()

	age := now.Sub(r.committedCT)
	if age < 0 {
		// The committed position cannot genuinely lead the present; an
		// uncalibrated host clock running behind the primary is not negative lag.
		return 0
	}

	return age
}

// closeStream closes and drops the current stream, if any. It always uses a
// fresh bounded context: closeStream runs on shutdown paths where the run ctx
// may already be canceled, and a killCursors on a dead context would never be
// delivered, leaking the server-side cursor until timeout.
//
// A failed close is informational, not actionable (the same contract as
// LeaderElector.Release): the local cursor is dropped regardless, and a lost
// killCursors leaks the server-side cursor only until the server's own
// cursor/session timeout reaps it. It is consumed here — logged at Warn —
// rather than returned: every caller would do exactly this and nothing else,
// so returning it would only relocate the same log line to five call sites.
func (r *Relay) closeStream() {
	if r.stream == nil {
		return
	}
	ctx, cancel := bound.Fresh()
	defer cancel()
	if err := r.stream.Close(ctx); err != nil {
		r.options.Logger.Warn("stream relay: close stream", "relay", r.name, "err", err)
	}
	r.stream = nil
}

func (r *Relay) sleep(ctx context.Context) {
	t := time.NewTimer(r.options.DrainWindow)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}
