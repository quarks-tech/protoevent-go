package stream

import (
	"context"
	"errors"
	"time"

	"github.com/quarks-tech/protoevent-go/pkg/transport/outbox"
)

// ErrStreamInvalidated is returned when the change stream emits an invalidate
// event (the outbox collection was dropped/renamed). Fatal.
var ErrStreamInvalidated = errors.New("stream: change stream invalidated")

// ErrHistoryLost is returned when the resume token has fallen off the oplog
// (ChangeStreamHistoryLost). Fatal in v1 — invoke the break-glass runbook.
var ErrHistoryLost = errors.New("stream: change stream history lost (resume token off oplog)")

// errLaneStopped signals that drainWindow stopped the lane on a send failure
// and closed the stream so the next RunOnce reopens and redelivers.
var errLaneStopped = errors.New("stream: lane stopped on send failure; will reopen and redeliver")

// Run drives the relay until ctx is canceled (returns ctx.Err()) or a fatal
// stream condition occurs (returns ErrStreamInvalidated / ErrHistoryLost).
// Releases leadership on exit so a planned shutdown fails over quickly.
func (r *Relay) Run(ctx context.Context) error {
	defer r.closeStream(context.Background())
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
			// redeliver next iteration. The send failure was already observed
			// via handleError, so don't re-observe here.
			r.sleep(ctx)
		case errors.Is(err, ErrStreamInvalidated), errors.Is(err, ErrHistoryLost):
			return err // fatal
		case ctx.Err() != nil:
			// Planned shutdown (ctx is genuinely done): the ctx.Err() check at
			// the top of the loop will return next iteration; don't report a
			// spurious error metric/log. Gated on run-context liveness, not
			// error identity — the mongo v2 driver's own operation timeouts
			// also surface as context.DeadlineExceeded, and those must still
			// be observed as real errors while ctx is alive (default branch).
			r.closeStream(ctx)
			r.sleep(ctx)
		default:
			// transient (leadership, reopen, send/save): report, drop the stream, retry
			r.options.Observer.ObserveError(r.name, err)
			r.options.Logger.Errorf("stream relay %q: %v", r.name, err)
			r.closeStream(ctx)
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
		r.closeStream(ctx)
		r.sleep(ctx) // avoid busy-spinning TryAcquireLeaderLock while not leader
		return nil
	}
	if r.stream == nil {
		token, ct, err := r.store.LoadToken(ctx, r.name)
		if err != nil {
			return err
		}
		s, err := r.store.Watch(ctx, token)
		if err != nil {
			return err
		}
		r.stream = s
		r.committedCT = ct
		if token == "" {
			// Fresh consumer group: establish a resume baseline from the stream's
			// initial resume token BEFORE draining, so a first-window send failure
			// (stop-the-lane, processed==0, which persists nothing) reopens from a
			// point preceding the failed event instead of restarting at a fresh
			// "now" and silently skipping it.
			if btok, bct := r.stream.PBRT(); btok != "" {
				if err := r.store.SaveToken(ctx, r.name, btok, bct); err != nil {
					return err
				}
				r.committedCT = bct
			}
		}
	}
	return r.drainWindow(ctx)
}

// drainWindow processes up to TokenBatchSize events (fast while buffered; one
// DrainWindow wait when idle), persists the token, and reports lag.
func (r *Relay) drainWindow(ctx context.Context) error {
	processed := 0
	stopped := false
	var lastTok string
	var lastCT time.Time

	for range r.options.TokenBatchSize {
		e, ok, err := r.stream.Next(ctx)
		if err != nil {
			return err
		}
		if !ok {
			break // window elapsed with no more events (caught up)
		}
		if e.Invalidate {
			return ErrStreamInvalidated
		}
		if sendErr := r.sender.Send(ctx, e.Message.Metadata, e.Message.Data); sendErr != nil {
			r.handleError(ctx, e.Message, sendErr)
			if r.options.ErrorHandler == nil {
				stopped = true
				break // stop-the-lane: do not advance past the failure
			}
			// park-and-continue: advance past the parked message
		}
		lastTok, lastCT = e.ResumeToken, e.ClusterTime
		processed++
	}

	switch {
	case processed > 0:
		if err := r.store.SaveToken(ctx, r.name, lastTok, lastCT); err != nil {
			return err
		}
		r.committedCT = lastCT
	case !stopped:
		// Empty window: persist PBRT so a caught-up-connected consumer stays
		// resumable and the lag anchor stays fresh (design §6c).
		if tok, ct := r.stream.PBRT(); tok != "" {
			if err := r.store.SaveToken(ctx, r.name, tok, ct); err != nil {
				return err
			}
			r.committedCT = ct
		}
	}

	more := stopped || processed == r.options.TokenBatchSize
	r.options.Observer.ObserveDrained(r.name, processed, r.committedTokenAge(), more)

	if stopped {
		// A change-stream cursor cannot rewind, so to actually redeliver the
		// failed event we must reopen from the last-persisted token. Close the
		// stream (next RunOnce reopens via LoadToken+Watch) and signal Run to
		// back off. Persisted state is already correct: processed>0 saved the
		// last success (reopen resumes just after it, at the failed event);
		// processed==0 saved nothing (reopen resumes from the prior token).
		r.closeStream(ctx)
		return errLaneStopped
	}
	return nil
}

// committedTokenAge is the cliff proxy: how far the committed token trails the
// oplog head (design §7). Cheap — no query.
func (r *Relay) committedTokenAge() time.Duration {
	if r.committedCT.IsZero() {
		return 0
	}
	return time.Since(r.committedCT)
}

func (r *Relay) closeStream(ctx context.Context) {
	if r.stream != nil {
		_ = r.stream.Close(ctx)
		r.stream = nil
	}
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
	r.options.Logger.Errorf("stream relay %q: send message %s: %v", r.name, msg.ID, err)
}
