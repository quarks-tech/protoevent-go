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

// Logger interface for relay error logging.
type Logger interface {
	Errorf(format string, args ...any)
}

type options struct {
	batchSize       int
	pollInterval    time.Duration
	maxRetries      int
	minRetryBackoff time.Duration
	retentionHours  int
	logger          Logger
	errorHandler    func(ctx context.Context, msg *outbox.Message, err error)

	// Leader election options
	leaderLockName string
	leaseTTL       time.Duration
}

func defaultOptions() options {
	return options{
		batchSize:       100,
		pollInterval:    time.Second,
		maxRetries:      3,
		minRetryBackoff: 15 * time.Second,
		retentionHours:  0,
		leaseTTL:        30 * time.Second,
	}
}

// Option configures the Relay.
type Option func(*options)

// WithBatchSize sets the maximum number of messages to fetch per poll.
func WithBatchSize(size int) Option {
	return func(o *options) {
		o.batchSize = size
	}
}

// WithPollInterval sets the interval between polling the outbox table.
func WithPollInterval(d time.Duration) Option {
	return func(o *options) {
		o.pollInterval = d
	}
}

// WithMaxRetries sets the maximum number of retry attempts before moving a message to parking lot.
func WithMaxRetries(maxRetries int) Option {
	return func(o *options) {
		o.maxRetries = maxRetries
	}
}

// WithMinRetryBackoff sets the minimum wait time before retrying a failed message.
func WithMinRetryBackoff(d time.Duration) Option {
	return func(o *options) {
		o.minRetryBackoff = d
	}
}

// WithRetentionHours sets the number of hours to retain messages before truncating partitions.
func WithRetentionHours(hours int) Option {
	return func(o *options) {
		o.retentionHours = hours
	}
}

// WithLogger sets the logger for relay errors.
func WithLogger(l Logger) Option {
	return func(o *options) {
		o.logger = l
	}
}

// WithErrorHandler sets a custom error handler for failed message sends.
func WithErrorHandler(handler func(ctx context.Context, msg *outbox.Message, err error)) Option {
	return func(o *options) {
		o.errorHandler = handler
	}
}

// WithLeaderElection enables leader election for running multiple relay instances.
// Only one instance will be active at a time. Requires the store to implement LeaderStore.
// The lockName should be unique per relay group (e.g., "outbox-relay-parkinglot").
func WithLeaderElection(lockName string) Option {
	return func(o *options) {
		o.leaderLockName = lockName
	}
}

// WithLeaseTTL sets the leader lock lease duration.
// The lock expires after this duration if not renewed. Default is 30 seconds.
func WithLeaseTTL(ttl time.Duration) Option {
	return func(o *options) {
		o.leaseTTL = ttl
	}
}

// Relay processes outbox messages with retry and parking lot support.
// Failed messages are moved to a wait queue and retried after backoff.
// After max retries, messages are moved to the parking lot.
type Relay struct {
	store    Store
	sender   eventbus.Sender
	options  options
	holderID string
}

// NewRelay creates a new message relay with parking lot support.
func NewRelay(store Store, sender eventbus.Sender, opts ...Option) *Relay {
	options := defaultOptions()
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
	cursor, err := r.store.GetOutboxCursor(ctx)
	if err != nil {
		return fmt.Errorf("get initial cursor: %w", err)
	}

	ticker := time.NewTicker(r.options.pollInterval)
	defer ticker.Stop()

	lastTruncateHour := -1

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			// If leader election is enabled, try to acquire/renew the lock
			if r.options.leaderLockName != "" {
				isLeader, err := r.tryAcquireLeadership(ctx)
				if err != nil {
					if r.options.logger != nil {
						r.options.logger.Errorf("leader election failed: %v", err)
					}
					continue
				}
				if !isLeader {
					continue
				}
			}

			// Move waiting messages back to outbox if their wait time has passed
			if _, err := r.store.MoveWaitingMessagesToOutbox(ctx); err != nil {
				if r.options.logger != nil {
					r.options.logger.Errorf("failed to move waiting messages to outbox: %v", err)
				}
			}

			// Truncate old partitions once per hour
			currentHour := time.Now().Hour()
			if currentHour != lastTruncateHour {
				r.truncateOldPartitions(ctx)
				lastTruncateHour = currentHour
			}

			newCursor, err := r.processBatch(ctx, cursor)
			if err != nil {
				if r.options.logger != nil {
					r.options.logger.Errorf("relay batch processing failed: %v", err)
				}
				continue
			}

			if newCursor != cursor {
				if err := r.store.SaveOutboxCursor(ctx, newCursor); err != nil {
					if r.options.logger != nil {
						r.options.logger.Errorf("failed to save cursor: %v", err)
					}
				}
				cursor = newCursor
			}
		}
	}
}

func (r *Relay) tryAcquireLeadership(ctx context.Context) (bool, error) {
	leaderStore, ok := r.store.(relay.LeaderStore)
	if !ok {
		return false, fmt.Errorf("store does not implement LeaderStore")
	}

	return leaderStore.TryAcquireLeaderLock(ctx, r.options.leaderLockName, r.holderID, r.options.leaseTTL)
}

func (r *Relay) processBatch(ctx context.Context, cursor string) (string, error) {
	messages, err := r.store.ListPendingOutboxMessages(ctx, cursor, r.options.batchSize)
	if err != nil {
		return cursor, fmt.Errorf("list pending messages: %w", err)
	}

	if len(messages) == 0 {
		return cursor, nil
	}

	for _, msg := range messages {
		select {
		case <-ctx.Done():
			return cursor, nil
		default:
			if msg.SentTime != nil {
				cursor = msg.ID
				continue
			}

			if err := r.sender.Send(ctx, msg.Metadata, msg.Data); err != nil {
				r.handleError(ctx, msg, err)
				cursor = msg.ID
				continue
			}

			if err := r.store.UpdateOutboxMessagesSentTime(ctx, time.Now(), msg.ID); err != nil {
				if r.options.logger != nil {
					r.options.logger.Errorf("failed to mark message %s as sent: %v", msg.ID, err)
				}
			}
			cursor = msg.ID
		}
	}

	return cursor, nil
}

func (r *Relay) handleError(ctx context.Context, msg *outbox.Message, err error) {
	if r.options.errorHandler != nil {
		r.options.errorHandler(ctx, msg, err)
	}

	if r.options.logger != nil {
		r.options.logger.Errorf("failed to send message %s: %v", msg.ID, err)
	}

	retryCount, incErr := r.store.IncrementOutboxMessageRetryCount(ctx, msg.ID)
	if incErr != nil {
		if r.options.logger != nil {
			r.options.logger.Errorf("failed to increment retry count for message %s: %v", msg.ID, incErr)
		}
		return
	}

	if retryCount >= r.options.maxRetries {
		reason := fmt.Sprintf("max retries (%d) exceeded: %v", r.options.maxRetries, err)
		if moveErr := r.store.MoveOutboxMessageToParkingLot(ctx, msg.ID, reason); moveErr != nil {
			if r.options.logger != nil {
				r.options.logger.Errorf("failed to move message %s to parking lot: %v", msg.ID, moveErr)
			}
		} else if r.options.logger != nil {
			r.options.logger.Errorf("message %s moved to parking lot: %s", msg.ID, reason)
		}
		return
	}

	// Move to wait queue for retry
	retryTime := time.Now().Add(r.options.minRetryBackoff)
	if moveErr := r.store.MoveOutboxMessageToWait(ctx, msg.ID, retryTime); moveErr != nil {
		if r.options.logger != nil {
			r.options.logger.Errorf("failed to move message %s to wait queue: %v", msg.ID, moveErr)
		}
	}
}

func (r *Relay) truncateOldPartitions(ctx context.Context) {
	if r.options.retentionHours <= 0 || r.options.retentionHours >= 24 {
		return
	}

	partitionedStore, ok := r.store.(relay.PartitionedStore)
	if !ok {
		return
	}

	currentHour := time.Now().Hour()
	partitions := getPartitionsToTruncate(currentHour, r.options.retentionHours)

	if len(partitions) == 0 {
		return
	}

	if err := partitionedStore.TruncateOutboxPartitions(ctx, partitions...); err != nil {
		if r.options.logger != nil {
			r.options.logger.Errorf("failed to truncate partitions: %v", err)
		}
	}
}

func getPartitionsToTruncate(currentHour, retentionHours int) []int {
	const totalPartitions = 24

	keep := make(map[int]struct{}, retentionHours+1)
	for i := range retentionHours + 1 {
		hour := (currentHour - i + totalPartitions) % totalPartitions
		keep[hour] = struct{}{}
	}

	toTruncate := make([]int, 0, totalPartitions-len(keep))
	for p := range totalPartitions {
		if _, ok := keep[p]; !ok {
			toTruncate = append(toTruncate, p)
		}
	}

	return toTruncate
}
