package stream

import (
	"context"
	"errors"
	"time"

	"github.com/quarks-tech/protoevent-go/pkg/transport/outbox"
	"github.com/quarks-tech/protoevent-go/pkg/transport/outbox/internal/notify"
	"github.com/quarks-tech/protoevent-go/pkg/transport/outbox/relay"
)

// shutdownTimeout bounds shutdown-path store I/O (closeStream's Stream.Close,
// saveToken's final persist) on a fresh context.Background(): those paths run
// where the run ctx may already be canceled, and a killCursors or token save
// on a dead context would never be delivered — mirroring internal/leader's
// releaseTimeout pattern.
const shutdownTimeout = 5 * time.Second

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
	defer func() {
		// Informational only: failover falls back to TTL expiry, and a store
		// outage also surfaces on the successor's TryAcquire (see Release doc).
		if err := r.leader.Release(); err != nil {
			r.options.Logger.Warn("stream relay: release leader lock", "relay", r.name, "err", err)
		}
		// Graceful release ends this instance's leadership: emit the
		// transition so telemetry does not leave a dead instance marked leader.
		r.trackLeadership(false)
	}()

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
			// via handleError, so don't re-observe here.
			r.sleep(ctx)
		case errors.Is(err, ErrInvalidated), errors.Is(err, ErrHistoryLost):
			// Fatal: report and stop. The deferred closeStream releases the cursor.
			notify.Error(r.options.Observer, r.name, err)
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
			notify.Error(r.options.Observer, r.name, err)
			r.options.Logger.Error("stream relay: pass failed", "relay", r.name, "err", err)
			r.closeStream()
			r.sleep(ctx)
		}
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
	r.trackLeadership(isLeader)
	if !isLeader {
		r.closeStream()
		r.sleep(ctx) // avoid busy-spinning TryAcquireLeaderLock while not leader
		return nil
	}
	if r.stream == nil {
		token, ct, err := r.store.LoadToken(ctx, r.name)
		if err != nil {
			return err
		}
		s, err := r.store.Watch(ctx, token, r.options.DrainWindow)
		if err != nil {
			return err
		}
		r.stream = s
		r.committedCT = ct
		r.lastSavedTok = token // the stored row already holds this position
		if token == "" {
			// Fresh consumer group: establish a resume baseline from the stream's
			// initial resume token BEFORE draining, so a first-window send failure
			// (stop-the-lane, processed==0, which persists nothing) reopens from a
			// point preceding the failed event instead of restarting at a fresh
			// "now" and silently skipping it.
			btok, bct, serverTime := r.stream.Checkpoint()
			if btok != "" {
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
				// Advance past the poison event ONLY on a confirmed park
				// (handler returned nil): persisting the token past it is
				// irreversible, and an unconfirmed DLQ write would silently
				// skip the event forever. Parking requires the event's resume
				// token: extraction is best-effort, and an empty one is
				// non-parkable — advancing with it would later persist "" and
				// erase the group's position (restart "at now", silently
				// skipping events).
				parkErr := r.handleError(ctx, r.options.PoisonHandler, &outbox.Message{ID: de.ID}, err)
				if parkErr == nil {
					// Poison event parked: resume past it, keeping the lane
					// moving. CommitTime extraction is best-effort too: a
					// zero one would (a) be swallowed by the store's monotone
					// guard, freezing the persisted position at the
					// pre-poison token forever (the lane reopens onto the
					// poison every window), and (b) zero committedCT,
					// silencing committedTokenAge()'s cliff alarm exactly
					// during a poison episode. Floor, never rewind: a poison
					// event must not drag lastCT below a commitTime an
					// earlier event in THIS window already advanced it to
					// (that would persist a stale anchor and inflate the
					// cliff metric); with nothing advanced yet, anchor on the
					// last committed commitTime — equal passes the monotone
					// $lte filter, so the token still advances.
					lastTok = de.ResumeToken
					switch {
					case de.CommitTime.After(lastCT):
						lastCT = de.CommitTime
					case lastCT.IsZero():
						lastCT = r.committedCT
					}
					advanced = true
					handled++
					r.noteSendOK(de.ResumeToken, de.ID)
					continue
				}
				// Unconfirmed park: fall through below and stop the lane at
				// the poison — same as having no handler or no token — and
				// retry the park on reopen. Join the park failure into the
				// error chain: telemetry alone (handleError observed and
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
				r.noteStuck(de.ResumeToken, de.ID, err)
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
		if sendErr := r.sender.Send(ctx, e.Message.Metadata, e.Message.Data); sendErr != nil {
			if ctx.Err() != nil {
				// The send failed because the run context was canceled (planned
				// shutdown), not because the event is bad: stop the lane without
				// parking it or advancing past it — it redelivers on restart.
				stopped = true
				break
			}
			// An event the caller's classifier calls PERMANENTLY unsendable is
			// parked like a poison event: redelivering it on every reopen can
			// never succeed, and it would wedge every event behind it. Only a
			// confirmed park (nil) advances the resume position past it; an
			// unconfirmed one stops the lane and retries the park on reopen.
			// Either way handleError has already reported this failure, so the
			// stop path below must not report it a second time — double-counting
			// OnError misrepresents the incident's size.
			if r.options.Unsendable != nil && r.options.Unsendable(sendErr) {
				parkErr := r.handleError(ctx, r.options.PoisonHandler, e.Message, sendErr)
				if parkErr == nil {
					lastTok, lastCT = e.ResumeToken, e.CommitTime
					advanced = true
					handled++
					r.noteSendOK(e.ResumeToken, e.Message.ID)
					continue
				}
				// Unsendable AND unparkable: the lane reopens onto this event
				// every window until the DLQ recovers, so the wedge escalates.
				r.noteStuck(e.ResumeToken, e.Message.ID, errors.Join(sendErr, parkErr))
				stopped = true
				break
			}
			// Any other send failure stops the lane, PoisonHandler or not: it is
			// downstream trouble (broker down, timeout), not an event fault,
			// and parking healthy events during an outage would bulk-divert
			// the backlog to the DLQ while advancing the persisted position
			// past it. The lane reopens at this event — order and delivery
			// preserved. (The nil handler keeps it out of the DLQ.)
			// nil handler: return is always nil (send failures are never parked).
			_ = r.handleError(ctx, nil, e.Message, sendErr)
			r.noteStuck(e.ResumeToken, e.Message.ID, sendErr)
			stopped = true
			break
		}
		sent++
		r.noteSendOK(e.ResumeToken, e.Message.ID)
		lastTok, lastCT = e.ResumeToken, e.CommitTime
		advanced = true
		handled++
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
	notify.Drained(r.options.Observer, r.name, sent, r.committedTokenAge(), more)

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
	if persisted {
		r.lastSavedTok = tok
		r.committedCT = ct
	}
	return nil
}

// stuckKey is the stuck-lane tracking key for one event: an existing string, never
// a newly built one, because Progress runs once per delivered event.
//
// The resume token identifies the position, but a poison event's token extraction
// is best-effort and can come back empty — and an empty token is exactly the case
// that wedges the lane hardest, since it is non-parkable. Keying every untokened
// event on the same value would make them indistinguishable: once one such episode
// escalated, the tracker would treat a LATER untokened poison event as the same
// still-unresolved one and never escalate again. Fall back to the event id, which is
// extracted independently.
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

// stuckLabel renders a key for the operator. Built only on escalation, so its
// allocation stays off the per-event path.
func stuckLabel(resumeToken, id string) string {
	switch {
	case resumeToken != "":
		return "resume token " + resumeToken
	case id != "":
		return "event " + id
	default:
		return "unidentifiable event"
	}
}

// noteSendOK ends a stuck episode after the event AT resumeToken was successfully
// disposed of (sent, or parked with confirmation). Keyed on the token, not
// unconditional: a window that re-delivers a prefix ahead of a wedged event —
// which is what a reopen does whenever the token save is being rejected by the
// store's monotone guard — would otherwise reset the escalation timer every
// window. See notify.StuckTracker.
func (r *Relay) noteSendOK(resumeToken, id string) {
	r.stuck.Progress(stuckKey(resumeToken, id))
}

// noteStuck escalates a lane that keeps stopping at the same resume token (see
// notify.StuckTracker). Must be called from EVERY path that stops the lane — a
// send failure, an unsendable event whose park was not confirmed, an unconfirmed
// poison park — since each of them wedges the stream identically: the cursor
// reopens onto the same event every window.
func (r *Relay) noteStuck(resumeToken, id string, err error) {
	stuckFor, escalate := r.stuck.Stuck(stuckKey(resumeToken, id))
	if !escalate {
		return
	}

	notify.StuckLane(r.options.Observer, r.options.Logger, "stream", r.name,
		stuckLabel(resumeToken, id), id, stuckFor, err)
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
// It must be measured against now, not against anything the relay has read.
// Anchoring it to the newest clusterTime seen from the cursor looks right and is
// exactly backwards: a relay six hours behind reads six-hour-old events, so that
// anchor equals the committed position and the gauge reads ~0 for the entire
// incident — silent in precisely the case the alarm exists for. A stop-the-lane
// outage has the same shape: the same failed event every window, nothing
// advancing.
//
// So: estimate the server's current time from a host clock corrected by an offset
// calibrated whenever the relay is caught up (calibrateClock), and subtract the
// committed clusterTime from that. The offset makes it skew-free — subtracting a
// server clusterTime from a raw host clock reports NEGATIVE ages on a pod whose
// clock trails the primary, which any gauge plots as ~0 — while the host clock
// supplies the passage of time that the stream itself stops providing during a
// backlog or an outage. Before the first calibration (a relay that has never
// caught up) it degrades to the raw host clock, which is right to within the skew.
//
// Because a clusterTime is truncated to whole seconds, sub-second lag reads as 0:
// the signal is a cliff warning measured in minutes, not a latency histogram.
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
	ctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	if err := r.stream.Close(ctx); err != nil {
		r.options.Logger.Warn("stream relay: close stream", "relay", r.name, "err", err)
	}
	r.stream = nil
}

// trackLeadership fires the OnLeadership signal + Info log on transitions
// only: without it a standby takeover or a stale leader resuming after a
// wedge leaves no trace in either instance's telemetry.
func (r *Relay) trackLeadership(isLeader bool) {
	if isLeader == r.wasLeader {
		return
	}
	r.wasLeader = isLeader
	notify.Leadership(r.options.Observer, r.options.Logger, "stream", r.name, isLeader)
}

func (r *Relay) sleep(ctx context.Context) {
	t := time.NewTimer(r.options.DrainWindow)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}

// handleError routes a genuine per-message failure to the shared relay error
// policy. h is the configured PoisonHandler for parkable poison events and nil
// for stop-the-lane send failures (observe+log only — the event will be
// redelivered, not parked). Returns the handler's park confirmation error
// (nil when h is nil).
func (r *Relay) handleError(ctx context.Context, h relay.PoisonHandler, msg *outbox.Message, err error) error {
	return notify.MessageFailure(ctx, h, r.options.Observer, r.options.Logger, "stream", r.name, msg, err)
}
