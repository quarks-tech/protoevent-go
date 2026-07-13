package stream

import (
	"context"
	"errors"
	"time"

	"github.com/quarks-tech/protoevent-go/pkg/transport/outbox"
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
func (r *Relay) RunOnce(ctx context.Context) error {
	leader, err := r.leader.TryAcquire(ctx)
	if err != nil {
		return err
	}
	if !leader {
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
			var de *DecodeError
			if errors.As(err, &de) && de.ResumeToken != "" && r.options.ErrorHandler != nil && ctx.Err() == nil {
				// Poison event: park it and resume past it, keeping the lane
				// moving. Requires the event's resume token: extraction is
				// best-effort, and an empty one is non-parkable — advancing
				// with it would later persist "" and erase the group's
				// position (restart "at now", silently skipping events).
				// Without a token or an ErrorHandler the error falls through
				// below and stops the lane without advancing past the event
				// (at-least-once, order preserved).
				r.handleError(ctx, &outbox.Message{ID: de.ID}, err)
				lastTok, lastCT = de.ResumeToken, de.ClusterTime
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
					return saveErr
				}
			}
			return err
		}
		if !ok {
			break // window elapsed with no more events (caught up)
		}
		if sendErr := r.sender.Send(ctx, e.Message.Metadata, e.Message.Data); sendErr != nil {
			if ctx.Err() != nil {
				// The send failed because the run context was canceled (planned
				// shutdown), not because the event is bad: stop the lane without
				// parking it or advancing past it — it redelivers on restart.
				stopped = true
				break
			}
			r.handleError(ctx, e.Message, sendErr)
			if r.options.ErrorHandler == nil {
				stopped = true
				break // stop-the-lane: do not advance past the failure
			}
			// park-and-continue: advance past the parked message
		} else {
			sent++
		}
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
// no-op-save skipping and lag reporting. When the run ctx is already canceled
// (the final save on a planned shutdown), the store call gets a fresh bounded
// context instead: a real store fails writes on a dead context, and losing
// that save would redeliver up to TokenBatchSize-1 acknowledged sends per
// deploy. Behavior on a live ctx is unchanged.
func (r *Relay) saveToken(ctx context.Context, tok string, ct time.Time) error {
	if ctx.Err() != nil {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
	}
	if err := r.store.SaveToken(ctx, r.name, tok, ct); err != nil {
		return err
	}
	r.lastSavedTok = tok
	r.committedCT = ct
	return nil
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

func (r *Relay) handleError(ctx context.Context, msg *outbox.Message, err error) {
	if r.options.ErrorHandler != nil {
		r.options.ErrorHandler(ctx, msg, err)
	}
	r.options.Observer.ObserveError(r.name, err)
	r.options.Logger.Error("stream relay: message failed", "relay", r.name, "event_id", msg.ID, "err", err)
}
