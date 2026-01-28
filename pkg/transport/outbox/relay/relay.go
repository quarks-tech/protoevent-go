package relay

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/quarks-tech/protoevent-go/pkg/eventbus"
	"github.com/quarks-tech/protoevent-go/pkg/transport/outbox"
)

// Relay reads messages from the pending table and forwards them to another transport.
// It processes messages in FIFO order and handles deleting or moving them after successful send.
//
// The relay uses a two-table approach:
//   - Pending table: messages waiting to be sent
//   - Completed table: messages that have been sent (optional, for audit)
//
// This eliminates cursor management and prevents race conditions with UUID v7 ordering.
type Relay struct {
	store    Store
	sender   eventbus.Sender
	options  Options
	holderID string
}

// NewRelay creates a new message relay.
//
// Parameters:
//   - store: The outbox store for reading pending messages
//   - sender: The transport sender to forward messages to (e.g., RabbitMQ sender)
//   - opts: Configuration options
func NewRelay(store Store, sender eventbus.Sender, opts ...Option) *Relay {
	options := DefaultOptions()
	for _, opt := range opts {
		opt(&options)
	}

	return &Relay{
		store:    store,
		sender:   sender,
		options:  options,
		holderID: uuid.NewString(),
	}
}

// Run starts the relay loop. It polls the pending table for messages
// and forwards them to the configured sender.
//
// The relay stops when the context is cancelled.
// Returns nil on graceful shutdown.
func (r *Relay) Run(ctx context.Context) error {
	ticker := time.NewTicker(r.options.PollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			// If leader election is enabled, try to acquire/renew the lock
			if r.options.LeaderLockName != "" {
				isLeader, err := r.tryAcquireLeadership(ctx)
				if err != nil {
					if r.options.Logger != nil {
						r.options.Logger.Errorf("leader election failed: %v", err)
					}
					continue
				}
				if !isLeader {
					continue
				}
			}

			if err := r.processBatch(ctx); err != nil {
				if r.options.Logger != nil {
					r.options.Logger.Errorf("relay batch processing failed: %v", err)
				}
			}
		}
	}
}

// RunOnce processes a single batch of messages and returns.
// Useful for testing or manual triggering.
func (r *Relay) RunOnce(ctx context.Context) error {
	return r.processBatch(ctx)
}

func (r *Relay) tryAcquireLeadership(ctx context.Context) (bool, error) {
	leaderStore, ok := r.store.(LeaderStore)
	if !ok {
		return false, fmt.Errorf("store does not implement LeaderStore")
	}

	return leaderStore.TryAcquireLeaderLock(ctx, r.options.LeaderLockName, r.holderID, r.options.LeaseTTL)
}

func (r *Relay) processBatch(ctx context.Context) error {
	messages, err := r.store.ListPendingMessages(ctx, r.options.BatchSize)
	if err != nil {
		return fmt.Errorf("list pending messages: %w", err)
	}

	if len(messages) == 0 {
		return nil
	}

	var sentIDs []string

	for _, msg := range messages {
		select {
		case <-ctx.Done():
			// Process any successfully sent messages before returning
			if len(sentIDs) > 0 {
				if err := r.markProcessed(ctx, sentIDs...); err != nil {
					return fmt.Errorf("mark processed on context done: %w", err)
				}
			}

			return nil
		default:
			if err := r.sender.Send(ctx, msg.Metadata, msg.Data); err != nil {
				// Process successfully sent messages before handling the error
				if len(sentIDs) > 0 {
					if batchErr := r.markProcessed(ctx, sentIDs...); batchErr != nil {
						return fmt.Errorf("mark processed before error: %w", batchErr)
					}
				}

				r.handleError(ctx, msg, fmt.Errorf("send message %s: %w", msg.ID, err))
				// Stop processing to maintain FIFO order
				return nil
			}
			sentIDs = append(sentIDs, msg.ID)
		}
	}

	// Batch process all successfully sent messages
	if len(sentIDs) > 0 {
		if err := r.markProcessed(ctx, sentIDs...); err != nil {
			return fmt.Errorf("mark processed: %w", err)
		}
	}

	return nil
}

func (r *Relay) markProcessed(ctx context.Context, ids ...string) error {
	switch r.options.ProcessingMode {
	case ProcessingModeDelete:
		return r.store.DeletePendingMessages(ctx, ids...)
	case ProcessingModeMove:
		return r.store.MovePendingToCompleted(ctx, time.Now(), ids...)
	}
	return nil
}

func (r *Relay) handleError(ctx context.Context, msg *outbox.Message, err error) {
	if r.options.ErrorHandler != nil {
		r.options.ErrorHandler(ctx, msg, err)
	}

	if r.options.Logger != nil {
		r.options.Logger.Errorf("failed to process message %s: %v", msg.ID, err)
	}
}
