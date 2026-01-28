package parkinglot

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/quarks-tech/protoevent-go/pkg/eventbus"
	"github.com/quarks-tech/protoevent-go/pkg/transport/outbox"
	"github.com/quarks-tech/protoevent-go/pkg/transport/outbox/relay"
)

// ParkingLotOptions extends relay.Options with retry-specific configuration.
type ParkingLotOptions struct {
	relay.Options
	MaxRetries      int
	MinRetryBackoff time.Duration
}

// DefaultParkingLotOptions returns the default parking lot relay configuration.
func DefaultParkingLotOptions() ParkingLotOptions {
	return ParkingLotOptions{
		Options:         relay.DefaultOptions(),
		MaxRetries:      3,
		MinRetryBackoff: 15 * time.Second,
	}
}

// Option configures parking lot relay options.
type Option func(*ParkingLotOptions)

// WithBatchSize sets the maximum number of messages to fetch per poll.
func WithBatchSize(size int) Option {
	return func(o *ParkingLotOptions) {
		o.BatchSize = size
	}
}

// WithPollInterval sets the interval between polling the pending table.
func WithPollInterval(d time.Duration) Option {
	return func(o *ParkingLotOptions) {
		o.PollInterval = d
	}
}

// WithProcessingMode sets how messages are handled after successful relay.
// relay.ProcessingModeDelete removes messages from pending table (default).
// relay.ProcessingModeMove moves messages to completed table for audit trail.
func WithProcessingMode(mode relay.ProcessingMode) Option {
	return func(o *ParkingLotOptions) {
		o.ProcessingMode = mode
	}
}

// WithMaxRetries sets the maximum number of retry attempts before moving a message to parking lot.
func WithMaxRetries(maxRetries int) Option {
	return func(o *ParkingLotOptions) {
		o.MaxRetries = maxRetries
	}
}

// WithMinRetryBackoff sets the minimum wait time before retrying a failed message.
func WithMinRetryBackoff(d time.Duration) Option {
	return func(o *ParkingLotOptions) {
		o.MinRetryBackoff = d
	}
}

// WithLogger sets the logger for relay errors.
func WithLogger(l relay.Logger) Option {
	return func(o *ParkingLotOptions) {
		o.Logger = l
	}
}

// WithErrorHandler sets a custom error handler for failed message sends.
func WithErrorHandler(handler func(ctx context.Context, msg *outbox.Message, err error)) Option {
	return func(o *ParkingLotOptions) {
		o.ErrorHandler = handler
	}
}

// WithLeaderElection enables leader election for running multiple relay instances.
// Only one instance will be active at a time. Requires the store to implement LeaderStore.
// The lockName should be unique per relay group (e.g., "outbox-relay-parkinglot").
func WithLeaderElection(lockName string) Option {
	return func(o *ParkingLotOptions) {
		o.LeaderLockName = lockName
	}
}

// WithLeaseTTL sets the leader lock lease duration.
// The lock expires after this duration if not renewed. Default is 30 seconds.
func WithLeaseTTL(ttl time.Duration) Option {
	return func(o *ParkingLotOptions) {
		o.LeaseTTL = ttl
	}
}

// Relay processes outbox messages with retry and parking lot support.
// Failed messages are moved to a wait table and retried after backoff.
// After max retries, messages are moved to the parking lot.
//
// The relay uses a four-table approach:
//   - Pending table: messages waiting to be sent
//   - Wait table: messages in retry backoff
//   - Parking lot table: permanently failed messages
//   - Completed table: successfully sent messages (optional, for audit)
//
// This eliminates cursor management and prevents race conditions with UUID v7 ordering.
type Relay struct {
	store    Store
	sender   eventbus.Sender
	options  ParkingLotOptions
	holderID string
}

// NewRelay creates a new message relay with parking lot support.
func NewRelay(store Store, sender eventbus.Sender, opts ...Option) *Relay {
	options := DefaultParkingLotOptions()
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

// Run starts the relay loop with parking lot support.
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

			// Move waiting messages back to pending if their wait time has passed
			if err := r.store.MoveWaitToPending(ctx); err != nil {
				if r.options.Logger != nil {
					r.options.Logger.Errorf("failed to move waiting messages to pending: %v", err)
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
	// Move waiting messages back to pending first
	if err := r.store.MoveWaitToPending(ctx); err != nil {
		return fmt.Errorf("move waiting messages: %w", err)
	}
	return r.processBatch(ctx)
}

func (r *Relay) tryAcquireLeadership(ctx context.Context) (bool, error) {
	leaderStore, ok := r.store.(relay.LeaderStore)
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

	for _, msg := range messages {
		select {
		case <-ctx.Done():
			return nil
		default:
			if err := r.sender.Send(ctx, msg.Metadata, msg.Data); err != nil {
				r.handleError(ctx, msg, err)
				continue
			}

			if err := r.markProcessed(ctx, msg); err != nil {
				if r.options.Logger != nil {
					r.options.Logger.Errorf("failed to mark message %s as processed: %v", msg.ID, err)
				}
			}
		}
	}

	return nil
}

func (r *Relay) markProcessed(ctx context.Context, msg *outbox.Message) error {
	switch r.options.ProcessingMode {
	case relay.ProcessingModeDelete:
		return r.store.DeletePendingMessages(ctx, msg.ID)
	case relay.ProcessingModeMove:
		return r.store.MovePendingToCompleted(ctx, time.Now(), msg.ID)
	}
	return nil
}

func (r *Relay) handleError(ctx context.Context, msg *outbox.Message, err error) {
	if r.options.ErrorHandler != nil {
		r.options.ErrorHandler(ctx, msg, err)
	}

	if r.options.Logger != nil {
		r.options.Logger.Errorf("failed to send message %s: %v", msg.ID, err)
	}

	// Use retry count from the message (populated by ListPendingMessages)
	retryCount := msg.RetryCount + 1

	if retryCount >= r.options.MaxRetries {
		reason := fmt.Sprintf("max retries (%d) exceeded: %v", r.options.MaxRetries, err)
		if moveErr := r.store.MovePendingToParkingLot(ctx, msg.ID, reason); moveErr != nil {
			if r.options.Logger != nil {
				r.options.Logger.Errorf("failed to move message %s to parking lot: %v", msg.ID, moveErr)
			}
		} else if r.options.Logger != nil {
			r.options.Logger.Errorf("message %s moved to parking lot: %s", msg.ID, reason)
		}
		return
	}

	// Move to wait table for retry
	retryTime := time.Now().Add(r.options.MinRetryBackoff)
	if moveErr := r.store.MovePendingToWait(ctx, msg.ID, retryTime, retryCount); moveErr != nil {
		if r.options.Logger != nil {
			r.options.Logger.Errorf("failed to move message %s to wait: %v", msg.ID, moveErr)
		}
	}
}
