package relay

import (
	"context"
	"time"

	"github.com/quarks-tech/protoevent-go/pkg/transport/outbox"
)

// Run starts the relay loop, polling for pending messages and forwarding them.
// It blocks until the context is canceled and returns the context error.
//
// If leader election is enabled (via WithLeaderElection), the relay will only
// process messages when it holds the leader lock. This allows running multiple
// relay instances for high availability while maintaining FIFO ordering.
func (r *Relay) Run(ctx context.Context) error {
	ticker := time.NewTicker(r.options.PollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if err := r.RunOnce(ctx); err != nil {
				if r.options.Logger != nil {
					r.options.Logger.Errorf("relay poll error: %v", err)
				}
			}
		}
	}
}

// RunOnce performs a single poll iteration: checks leadership (if enabled),
// fetches pending messages, and forwards them to the sender.
// Returns nil if no messages were processed or all messages were sent successfully.
func (r *Relay) RunOnce(ctx context.Context) error {
	isLeader, err := r.tryAcquireLeadership(ctx)
	if err != nil {
		return err
	}

	if !isLeader {
		return nil
	}

	return r.processBatch(ctx)
}

// tryAcquireLeadership attempts to acquire or renew the leader lock.
// If leader election is not configured, always returns true.
func (r *Relay) tryAcquireLeadership(ctx context.Context) (bool, error) {
	if r.options.LeaderLockName == "" {
		return true, nil
	}

	leaderStore, ok := r.store.(LeaderStore)
	if !ok {
		return true, nil
	}

	return leaderStore.TryAcquireLeaderLock(ctx, r.options.LeaderLockName, r.holderID, r.options.LeaseTTL)
}

// processBatch fetches pending messages and sends them to the configured sender.
// Messages are processed in FIFO order. Successfully sent messages are marked as completed.
func (r *Relay) processBatch(ctx context.Context) error {
	messages, err := r.store.ListPendingMessages(ctx, r.options.BatchSize)
	if err != nil {
		return err
	}

	if len(messages) == 0 {
		return nil
	}

	completedIDs := make([]string, 0, len(messages))
	sentTime := time.Now()

	for _, msg := range messages {
		if err := r.sendMessage(ctx, msg); err != nil {
			r.handleError(ctx, msg, err)
			continue
		}

		completedIDs = append(completedIDs, msg.ID)
	}

	if len(completedIDs) > 0 {
		if err := r.store.CompletePendingMessages(ctx, sentTime, completedIDs...); err != nil {
			return err
		}
	}

	return nil
}

// sendMessage sends a single message to the configured sender.
func (r *Relay) sendMessage(ctx context.Context, msg *outbox.Message) error {
	return r.sender.Send(ctx, msg.Metadata, msg.Data)
}

// handleError handles a message send error.
// If an error handler is configured, it is called. Otherwise, the error is logged.
func (r *Relay) handleError(ctx context.Context, msg *outbox.Message, err error) {
	if r.options.ErrorHandler != nil {
		r.options.ErrorHandler(ctx, msg, err)
		return
	}

	if r.options.Logger != nil {
		r.options.Logger.Errorf("failed to send message %s: %v", msg.ID, err)
	}
}
