package outbox

import (
	"context"
	"fmt"
	"time"

	"github.com/quarks-tech/protoevent-go/pkg/eventbus"
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
	Errorf(format string, args ...interface{})
}

type relayOptions struct {
	batchSize       int
	pollInterval    time.Duration
	processingMode  ProcessingMode
	batchProcessing bool
	logger          Logger
	errorHandler    func(ctx context.Context, msg *Message, err error)
}

func defaultRelayOptions() relayOptions {
	return relayOptions{
		batchSize:       100,
		pollInterval:    time.Second,
		processingMode:  ProcessingModeDelete,
		batchProcessing: false,
	}
}

// RelayOption configures the Relay.
type RelayOption func(*relayOptions)

// WithBatchSize sets the maximum number of messages to fetch per poll.
func WithBatchSize(size int) RelayOption {
	return func(o *relayOptions) {
		o.batchSize = size
	}
}

// WithPollInterval sets the interval between polling the outbox table.
func WithPollInterval(d time.Duration) RelayOption {
	return func(o *relayOptions) {
		o.pollInterval = d
	}
}

// WithProcessingMode sets how messages are handled after successful relay.
func WithProcessingMode(mode ProcessingMode) RelayOption {
	return func(o *relayOptions) {
		o.processingMode = mode
	}
}

// WithBatchProcessing enables batch processing for marking messages as sent or deleting them.
// When enabled, the relay will collect all successfully sent message IDs and process them
// in a single batch operation at the end of each poll cycle.
func WithBatchProcessing() RelayOption {
	return func(o *relayOptions) {
		o.batchProcessing = true
	}
}

// WithLogger sets the logger for relay errors.
func WithLogger(l Logger) RelayOption {
	return func(o *relayOptions) {
		o.logger = l
	}
}

// WithErrorHandler sets a custom error handler for failed message sends.
// If not set, errors are logged and the relay continues with the next message.
func WithErrorHandler(handler func(ctx context.Context, msg *Message, err error)) RelayOption {
	return func(o *relayOptions) {
		o.errorHandler = handler
	}
}

// Relay reads messages from the outbox and forwards them to another transport.
// It processes messages in FIFO order and handles marking them as sent or deleting them.
type Relay struct {
	store   RelayStore
	sender  eventbus.Sender
	options relayOptions
}

// NewRelay creates a new message relay.
//
// Parameters:
//   - store: The outbox store for reading pending messages
//   - sender: The transport sender to forward messages to (e.g., RabbitMQ sender)
//   - opts: Configuration options
func NewRelay(store RelayStore, sender eventbus.Sender, opts ...RelayOption) *Relay {
	options := defaultRelayOptions()
	for _, opt := range opts {
		opt(&options)
	}

	return &Relay{
		store:   store,
		sender:  sender,
		options: options,
	}
}

// Run starts the relay loop. It polls the outbox for pending messages
// and forwards them to the configured sender.
//
// The relay stops when the context is cancelled.
// Returns nil on graceful shutdown, or an error if polling fails.
func (r *Relay) Run(ctx context.Context) error {
	cursor, err := r.store.GetCursor(ctx)
	if err != nil {
		return fmt.Errorf("get initial cursor: %w", err)
	}

	ticker := time.NewTicker(r.options.pollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			newCursor, err := r.processBatch(ctx, cursor)
			if err != nil {
				if r.options.logger != nil {
					r.options.logger.Errorf("relay batch processing failed: %v", err)
				}

				continue
			}

			cursor = newCursor
		}
	}
}

// RunOnce processes a single batch of messages and returns.
// Useful for testing or manual triggering.
func (r *Relay) RunOnce(ctx context.Context) error {
	cursor, err := r.store.GetCursor(ctx)
	if err != nil {
		return fmt.Errorf("get cursor: %w", err)
	}

	_, err = r.processBatch(ctx, cursor)
	return err
}

func (r *Relay) processBatch(ctx context.Context, cursor string) (string, error) {
	messages, err := r.store.ListPendingMessages(ctx, cursor, r.options.batchSize)
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

func (r *Relay) processBatchSequentially(ctx context.Context, cursor string, messages []*Message) (string, error) {
	for _, msg := range messages {
		select {
		case <-ctx.Done():
			return cursor, nil
		default:
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

func (r *Relay) processBatchWithBatching(ctx context.Context, cursor string, messages []*Message) (string, error) {
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
		return r.store.DeleteMessages(ctx, ids...)
	case ProcessingModeMarkSent:
		return r.store.UpdateMessagesSentTime(ctx, time.Now(), ids...)
	}
	return nil
}

func (r *Relay) processMessage(ctx context.Context, msg *Message) error {
	// Send to the downstream transport
	if err := r.sender.Send(ctx, msg.Metadata, msg.Data); err != nil {
		return fmt.Errorf("send message %s: %w", msg.ID, err)
	}

	// Mark as processed
	return r.markProcessed(ctx, msg.ID)
}

func (r *Relay) handleError(ctx context.Context, msg *Message, err error) {
	if r.options.errorHandler != nil {
		r.options.errorHandler(ctx, msg, err)

		return
	}

	if r.options.logger != nil {
		r.options.logger.Errorf("failed to process message %s: %v", msg.ID, err)
	}
}
