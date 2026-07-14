package stream

import (
	"context"
	"errors"
	"time"

	"github.com/quarks-tech/protoevent-go/pkg/transport/outbox"
	"github.com/quarks-tech/protoevent-go/pkg/transport/outbox/relay"
)

// shutdownTimeout bounds shutdown-path store I/O (closeStream's Stream.Close,
// saveToken's final persist) on a fresh context.Background(): those paths run
// where the run ctx may already be canceled, and a killCursors or token save
// on a dead context would never be delivered — mirroring relay/leader.go's
// releaseTimeout pattern.
const shutdownTimeout = 5 * time.Second

// errLaneStopped signals that drainWindow stopped the lane (send failure or
// planned shutdown) and closed the stream so the next RunOnce reopens and
// redelivers.
var errLaneStopped = errors.New("stream: lane stopped; will reopen and redeliver")

// Run drives the relay until ctx is canceled (returns ctx.Err()) or a fatal
// stream condition occurs (returns ErrStreamInvalidated / ErrHistoryLost).
// Releases leadership on exit so a planned shutdown fails over quickly.
func (r *Relay) Run(ctx context.Context) error {
	defer r.closeStream()
	defer r.leader.Release()

	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		err := r.RunOnce(ctx)
		switch {
		case err == nil:
			// keep looping; RunOnce already blocked ~one drain window
		case errors.Is(err, errLaneStopped):
			// Stream already closed by drainWindow; back off, then reopen and
			// redeliver next iteration. A send failure was already observed
			// via handleError, so don't re-observe here.
			r.sleep(ctx)
		case errors.Is(err, ErrStreamInvalidated), errors.Is(err, ErrHistoryLost):
			// Fatal: report and stop. The deferred closeStream releases the cursor.
			r.options.Observer.ObserveError(r.name, err)
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
			r.options.Observer.ObserveError(r.name, err)
			r.options.Logger.Error("stream relay: pass failed", "relay", r.name, "err", err)
			r.closeStream()
			r.sleep(ctx)
		}
	}
}

// RunOnce performs one leader-gated drain window. Non-leaders return nil without
// touching the stream. Exposed for tests; Run calls it in a loop.
//
// The setup store calls here (TryAcquire, LoadToken, Watch) are bounded by
// LeaseTTL via opCtx: without a deadline, a Mongo call that wedges at the
// network layer would block this single Run goroutine forever — the lease
// silently expires, a standby takes over, and this instance neither errors
// nor logs (a silent permanent stall), then resumes as a stale leader when
// the call finally unblocks. Beyond LeaseTTL the call's success is moot
// anyway: the lease is already lost.
func (r *Relay) RunOnce(ctx context.Context) error {
	leader, err := r.tryAcquireBounded(ctx)
	if err != nil {
		return err
	}
	if !leader {
		r.closeStream()
		r.sleep(ctx) // avoid busy-spinning TryAcquireLeaderLock while not leader
		return nil
	}
	if r.stream == nil {
		token, ct, err := r.loadTokenBounded(ctx)
		if err != nil {
			return err
		}
		s, err := r.watchBounded(ctx, token)
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
			if btok, bct := r.stream.PBRT(); btok != "" {
				if err := r.saveToken(ctx, btok, bct); err != nil {
					return err
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
			if de, isPoison := errors.AsType[*DecodeError](err); isPoison && de.ResumeToken != "" && r.options.ErrorHandler != nil && ctx.Err() == nil {
				// Poison event: park it and resume past it, keeping the lane
				// moving. Requires the event's resume token: extraction is
				// best-effort, and an empty one is non-parkable — advancing
				// with it would later persist "" and erase the group's
				// position (restart "at now", silently skipping events).
				// Without a token or an ErrorHandler the error falls through
				// below and stops the lane without advancing past the event
				// (at-least-once, order preserved).
				r.handleError(ctx, r.options.ErrorHandler, &outbox.Message{ID: de.ID}, err)
				lastTok = de.ResumeToken
				// ClusterTime extraction is best-effort too: a zero one would
				// (a) be swallowed by the store's monotone guard, freezing the
				// persisted position at the pre-poison token forever (the lane
				// reopens onto the poison every window), and (b) zero
				// committedCT, silencing committedTokenAge()'s cliff alarm
				// exactly during a poison episode. Anchor on the last
				// committed clusterTime instead: equal passes the monotone
				// $lte filter, so the token still advances.
				if lastCT = de.ClusterTime; lastCT.IsZero() {
					lastCT = r.committedCT
				}
				advanced = true
				handled++
				continue
			}
			// Non-parkable Next error: persist the sent prefix's position
			// first (the same save the send-failure stop path gets via
			// `case advanced:` below), so Run's close/reopen/backoff resumes
			// after the last success — at the poison — instead of re-sending
			// the prefix every DrainWindow.
			if advanced {
				if saveErr := r.saveToken(ctx, lastTok, lastCT); saveErr != nil {
					// Join rather than replace: returning only saveErr would
					// launder a fatal Next error (ErrStreamInvalidated /
					// ErrHistoryLost) into a transient one for this pass, and
					// Run would do a spurious close/backoff/reopen before the
					// fatal resurfaced.
					return errors.Join(err, saveErr)
				}
			}
			return err
		}
		if !ok {
			break // window elapsed with no more events (caught up)
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
			// A send failure ALWAYS stops the lane, ErrorHandler or not: it is
			// downstream trouble (broker down, timeout), not an event fault,
			// and parking healthy events during an outage would bulk-divert
			// the backlog to the DLQ while advancing the persisted position
			// past it. The lane reopens at this event — order and delivery
			// preserved. (The nil handler keeps it out of the DLQ.)
			r.handleError(ctx, nil, e.Message, sendErr)
			stopped = true
			break
		}
		sent++
		lastTok, lastCT = e.ResumeToken, e.ClusterTime
		advanced = true
		handled++
	}

	switch {
	case advanced:
		if err := r.saveToken(ctx, lastTok, lastCT); err != nil {
			return err
		}
	case !stopped:
		// Empty window: persist PBRT so a caught-up-connected consumer stays
		// resumable and the lag anchor stays fresh. Skip the write
		// when the token is unchanged since the last save (an idle stream would
		// otherwise persist the same position every DrainWindow) — the lag
		// bookkeeping still advances locally.
		if tok, ct := r.stream.PBRT(); tok != "" {
			if tok == r.lastSavedTok {
				r.committedCT = ct
			} else if err := r.saveToken(ctx, tok, ct); err != nil {
				return err
			}
		}
	}

	more := stopped || handled == r.options.TokenBatchSize
	r.options.Observer.ObserveDrained(r.name, sent, r.committedTokenAge(), more)

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
		return errLaneStopped
	}
	return nil
}

// saveToken persists the position and updates the local trackers used for
// no-op-save skipping and lag reporting — but ONLY when the store confirms it
// persisted: a save the monotone guard classified as stale (persisted=false,
// e.g. a stale leader finishing a slow window) must not advance
// lastSavedTok/committedCT, or committedTokenAge() — the resume-token-cliff
// early warning — would report freshness the stored row doesn't have.
//
// The store call is bounded via relay.BoundContext: by LeaseTTL while the run
// ctx is alive (a wedged network call must not silently stall the relay past
// its own lease), and by shutdownTimeout on a fresh context once the run ctx
// is canceled (the final save on a planned shutdown — a real store fails
// writes on a dead context, and losing that save would redeliver up to
// TokenBatchSize-1 acknowledged sends per deploy).
func (r *Relay) saveToken(ctx context.Context, tok string, ct time.Time) error {
	ctx, cancel := relay.BoundContext(ctx, r.options.LeaseTTL, shutdownTimeout)
	defer cancel()
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

// tryAcquireBounded, loadTokenBounded, and watchBounded bound the setup store
// calls by LeaseTTL (see RunOnce's doc). The Watch bound covers only opening
// the stream (the initial aggregate); the returned cursor is not tied to it.
func (r *Relay) tryAcquireBounded(ctx context.Context) (bool, error) {
	ctx, cancel := relay.BoundContext(ctx, r.options.LeaseTTL, shutdownTimeout)
	defer cancel()
	return r.leader.TryAcquire(ctx)
}

func (r *Relay) loadTokenBounded(ctx context.Context) (string, time.Time, error) {
	ctx, cancel := relay.BoundContext(ctx, r.options.LeaseTTL, shutdownTimeout)
	defer cancel()
	return r.store.LoadToken(ctx, r.name)
}

func (r *Relay) watchBounded(ctx context.Context, token string) (Stream, error) {
	ctx, cancel := relay.BoundContext(ctx, r.options.LeaseTTL, shutdownTimeout)
	defer cancel()
	return r.store.Watch(ctx, token, r.options.DrainWindow)
}

// committedTokenAge is the cliff proxy: how far the committed token trails the
// oplog head — the resume-token-cliff early-warning signal. Cheap — no query.
func (r *Relay) committedTokenAge() time.Duration {
	if r.committedCT.IsZero() {
		return 0
	}
	return time.Since(r.committedCT)
}

// closeStream closes and drops the current stream, if any. It always uses a
// fresh bounded context: closeStream runs on shutdown paths where the run ctx
// may already be canceled, and a killCursors on a dead context would never be
// delivered, leaking the server-side cursor until timeout.
func (r *Relay) closeStream() {
	if r.stream == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	_ = r.stream.Close(ctx)
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

// handleError routes a genuine per-message failure to the shared relay error
// policy. h is the configured ErrorHandler for parkable poison events and nil
// for stop-the-lane send failures (observe+log only — the event will be
// redelivered, not parked).
func (r *Relay) handleError(ctx context.Context, h relay.ErrorHandler, msg *outbox.Message, err error) {
	relay.HandleMessageError(ctx, h, r.options.Observer, r.options.Logger, "stream", r.name, msg, err)
}
