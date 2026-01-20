package relay

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/quarks-tech/protoevent-go/pkg/eventbus"
	"github.com/quarks-tech/protoevent-go/pkg/transport/outbox"
)

// ProcessingMode determines how the relay handles successfully sent messages.
type ProcessingMode int

const (
	// ProcessingModeDelete removes messages from the outbox after successful relay.
	ProcessingModeDelete ProcessingMode = iota

	// ProcessingModeMarkSent updates the sent_time timestamp, keeping messages for audit.
	ProcessingModeMarkSent
)

// Logger interface for relay error logging.
type Logger interface {
	Errorf(format string, args ...any)
}

type options struct {
	batchSize       int
	pollInterval    time.Duration
	processingMode  ProcessingMode
	batchProcessing bool
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
		processingMode:  ProcessingModeMarkSent,
		batchProcessing: false,
		retentionHours:  0, // disabled by default
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

// WithProcessingMode sets how messages are handled after successful relay.
func WithProcessingMode(mode ProcessingMode) Option {
	return func(o *options) {
		o.processingMode = mode
	}
}

// WithBatchProcessing enables batch processing for marking messages as sent or deleting them.
// When enabled, the relay will collect all successfully sent message IDs and process them
// in a single batch operation at the end of each poll cycle.
func WithBatchProcessing() Option {
	return func(o *options) {
		o.batchProcessing = true
	}
}

// WithRetentionHours sets the number of hours to retain messages before truncating partitions.
// When set > 0, the relay will periodically truncate hourly partitions older than the retention period.
// Requires the outbox table to be partitioned by HASH(HOUR(create_time)) PARTITIONS 24.
// Set to 0 to disable (default).
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
// If not set, errors are logged and the relay continues with the next message.
func WithErrorHandler(handler func(ctx context.Context, msg *outbox.Message, err error)) Option {
	return func(o *options) {
		o.errorHandler = handler
	}
}

// WithLeaderElection enables leader election for running multiple relay instances.
// Only one instance will be active at a time. Requires the store to implement LeaderStore.
// The lockName should be unique per relay group (e.g., "outbox-relay").
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

// Relay reads messages from the outbox and forwards them to another transport.
// It processes messages in FIFO order and handles marking them as sent or deleting them.
type Relay struct {
	store    Store
	sender   eventbus.Sender
	options  options
	holderID string
}

// NewRelay creates a new message relay.
//
// Parameters:
//   - store: The outbox store for reading pending messages
//   - sender: The transport sender to forward messages to (e.g., RabbitMQ sender)
//   - opts: Configuration options
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

// Run starts the relay loop. It polls the outbox for pending messages
// and forwards them to the configured sender.
//
// The relay stops when the context is cancelled.
// Returns nil on graceful shutdown, or an error if polling fails.
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

// RunOnce processes a single batch of messages and returns.
// Useful for testing or manual triggering.
func (r *Relay) RunOnce(ctx context.Context) error {
	cursor, err := r.store.GetOutboxCursor(ctx)
	if err != nil {
		return fmt.Errorf("get cursor: %w", err)
	}

	_, err = r.processBatch(ctx, cursor)
	return err
}

func (r *Relay) tryAcquireLeadership(ctx context.Context) (bool, error) {
	leaderStore, ok := r.store.(LeaderStore)
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

	if r.options.batchProcessing {
		return r.processBatchWithBatching(ctx, cursor, messages)
	}

	return r.processBatchSequentially(ctx, cursor, messages)
}

func (r *Relay) processBatchSequentially(ctx context.Context, cursor string, messages []*outbox.Message) (string, error) {
	for _, msg := range messages {
		select {
		case <-ctx.Done():
			return cursor, nil
		default:
			// Skip already sent messages
			if msg.SentTime != nil {
				cursor = msg.ID
				continue
			}

			if err := r.processMessage(ctx, msg); err != nil {
				r.handleError(ctx, msg, err)
				// Stop processing to maintain FIFO order
				// The failed message will be retried in the next batch
				return cursor, nil
			}
			cursor = msg.ID
		}
	}

	return cursor, nil
}

func (r *Relay) processBatchWithBatching(ctx context.Context, cursor string, messages []*outbox.Message) (string, error) {
	var sentIDs []string

	for _, msg := range messages {
		select {
		case <-ctx.Done():
			// Process any successfully sent messages before returning
			if len(sentIDs) > 0 {
				if err := r.markProcessed(ctx, sentIDs...); err != nil {
					return cursor, fmt.Errorf("mark processed on context done: %w", err)
				}
			}

			return cursor, nil
		default:
			// Skip already sent messages
			if msg.SentTime != nil {
				cursor = msg.ID
				continue
			}

			if err := r.sender.Send(ctx, msg.Metadata, msg.Data); err != nil {
				// Process successfully sent messages before handling the error
				if len(sentIDs) > 0 {
					if batchErr := r.markProcessed(ctx, sentIDs...); batchErr != nil {
						return cursor, fmt.Errorf("mark processed before error: %w", batchErr)
					}
					cursor = sentIDs[len(sentIDs)-1]
				}

				r.handleError(ctx, msg, fmt.Errorf("send message %s: %w", msg.ID, err))
				// Stop processing to maintain FIFO order
				return cursor, nil
			}
			sentIDs = append(sentIDs, msg.ID)
		}
	}

	// Batch process all successfully sent messages
	if len(sentIDs) > 0 {
		if err := r.markProcessed(ctx, sentIDs...); err != nil {
			return cursor, fmt.Errorf("mark processed: %w", err)
		}
		cursor = sentIDs[len(sentIDs)-1]
	}

	return cursor, nil
}

func (r *Relay) markProcessed(ctx context.Context, ids ...string) error {
	switch r.options.processingMode {
	case ProcessingModeDelete:
		return r.store.DeleteOutboxMessages(ctx, ids...)
	case ProcessingModeMarkSent:
		return r.store.UpdateOutboxMessagesSentTime(ctx, time.Now(), ids...)
	}
	return nil
}

func (r *Relay) processMessage(ctx context.Context, msg *outbox.Message) error {
	// Send to the downstream transport
	if err := r.sender.Send(ctx, msg.Metadata, msg.Data); err != nil {
		return fmt.Errorf("send message %s: %w", msg.ID, err)
	}

	// Mark as processed
	return r.markProcessed(ctx, msg.ID)
}

func (r *Relay) handleError(ctx context.Context, msg *outbox.Message, err error) {
	if r.options.errorHandler != nil {
		r.options.errorHandler(ctx, msg, err)
	}

	if r.options.logger != nil {
		r.options.logger.Errorf("failed to process message %s: %v", msg.ID, err)
	}
}

// truncateOldPartitions truncates hourly partitions outside the retention window.
// With HASH(HOUR(create_time)) PARTITIONS 24, partitions are p0-p23.
func (r *Relay) truncateOldPartitions(ctx context.Context) {
	if r.options.retentionHours <= 0 || r.options.retentionHours >= 24 {
		return
	}

	partitionedStore, ok := r.store.(PartitionedStore)
	if !ok {
		return
	}

	currentHour := time.Now().Hour()
	partitionsToTruncate := r.getPartitionsToTruncate(currentHour)

	if len(partitionsToTruncate) == 0 {
		return
	}

	if err := partitionedStore.TruncateOutboxPartitions(ctx, partitionsToTruncate...); err != nil {
		if r.options.logger != nil {
			r.options.logger.Errorf("failed to truncate partitions: %v", err)
		}
	}
}

// getPartitionsToTruncate returns partition numbers (0-23) that are outside the retention window.
func (r *Relay) getPartitionsToTruncate(currentHour int) []int {
	const totalPartitions = 24

	// Keep partitions from (currentHour - retentionHours) to currentHour (inclusive)
	keep := make(map[int]struct{}, r.options.retentionHours+1)
	for i := range r.options.retentionHours + 1 {
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
