package stream

import (
	"context"
	"errors"
	"time"

	"github.com/quarks-tech/protoevent-go/pkg/transport/outbox"
	"github.com/quarks-tech/protoevent-go/pkg/transport/outbox/relay"
)

const releaseTimeout = 5 * time.Second

// ErrStreamInvalidated is returned when the change stream emits an invalidate
// event (the outbox collection was dropped/renamed). Fatal.
var ErrStreamInvalidated = errors.New("stream: change stream invalidated")

// ErrHistoryLost is returned when the resume token has fallen off the oplog
// (ChangeStreamHistoryLost). Fatal in v1 — invoke the break-glass runbook.
var ErrHistoryLost = errors.New("stream: change stream history lost (resume token off oplog)")

// Run drives the relay until ctx is canceled (returns ctx.Err()) or a fatal
// stream condition occurs (returns ErrStreamInvalidated / ErrHistoryLost).
// Releases leadership on exit so a planned shutdown fails over quickly.
func (r *Relay) Run(ctx context.Context) error {
	defer r.closeStream(context.Background())
	defer r.releaseLeadership()

	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		err := r.RunOnce(ctx)
		switch {
		case err == nil:
			// keep looping; RunOnce already blocked ~one drain window
		case errors.Is(err, ErrStreamInvalidated), errors.Is(err, ErrHistoryLost):
			return err // fatal
		default:
			// transient (leadership, reopen, send/save): report, drop the stream, retry
			r.options.Observer.ObserveError(r.name, err)
			if r.options.Logger != nil {
				r.options.Logger.Errorf("stream relay %q: %v", r.name, err)
			}
			r.closeStream(ctx)
			r.sleep(ctx)
		}
	}
}

// RunOnce performs one leader-gated drain window. Non-leaders return nil without
// touching the stream. Exposed for tests; Run calls it in a loop.
func (r *Relay) RunOnce(ctx context.Context) error {
	leader, err := r.tryAcquireLeadership(ctx)
	if err != nil {
		return err
	}
	if !leader {
		r.closeStream(ctx)
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

	for i := 0; i < r.options.TokenBatchSize; i++ {
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

	more := processed == r.options.TokenBatchSize && !stopped
	r.options.Observer.ObserveDrained(r.name, processed, r.committedTokenAge(), more)
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

func (r *Relay) tryAcquireLeadership(ctx context.Context) (bool, error) {
	ls, ok := r.store.(relay.LeaderStore)
	if !ok {
		r.isLeader = true
		return true, nil
	}
	held, err := ls.TryAcquireLeaderLock(ctx, r.options.LeaderLockName, r.holderID, r.options.LeaseTTL)
	if err != nil {
		return false, err
	}
	r.isLeader = held
	return held, nil
}

func (r *Relay) releaseLeadership() {
	ls, ok := r.store.(relay.LeaderStore)
	if !ok || !r.isLeader {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), releaseTimeout)
	defer cancel()
	_ = ls.ReleaseLeaderLock(ctx, r.options.LeaderLockName, r.holderID)
	r.isLeader = false
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
	if r.options.Logger != nil {
		r.options.Logger.Errorf("stream relay %q: send message %s: %v", r.name, msg.ID, err)
	}
}
