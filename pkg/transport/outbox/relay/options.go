package relay

import (
	"context"
	"time"

	"github.com/quarks-tech/protoevent-go/pkg/transport/outbox"
)

// ProcessingMode determines how the relay handles successfully sent messages.
type ProcessingMode int

const (
	// ProcessingModeDelete removes messages from the pending table after successful relay.
	// Use this when you don't need an audit trail.
	ProcessingModeDelete ProcessingMode = iota

	// ProcessingModeMove moves messages from pending to completed table after successful relay.
	// Use this when you need an audit trail of sent messages.
	ProcessingModeMove
)

// Logger interface for relay error logging.
type Logger interface {
	Errorf(format string, args ...any)
}

// Options contains configuration for relay instances.
// This is exported to allow parkinglot relay to embed and extend it.
type Options struct {
	BatchSize      int
	PollInterval   time.Duration
	ProcessingMode ProcessingMode
	Logger         Logger
	ErrorHandler   func(ctx context.Context, msg *outbox.Message, err error)

	// Leader election options
	LeaderLockName string
	LeaseTTL       time.Duration
}

// DefaultOptions returns the default relay configuration.
func DefaultOptions() Options {
	return Options{
		BatchSize:      100,
		PollInterval:   time.Second,
		ProcessingMode: ProcessingModeDelete,
		LeaseTTL:       30 * time.Second,
	}
}

// Option configures relay options.
type Option func(*Options)

// WithBatchSize sets the maximum number of messages to fetch per poll.
func WithBatchSize(size int) Option {
	return func(o *Options) {
		o.BatchSize = size
	}
}

// WithPollInterval sets the interval between polling the pending table.
func WithPollInterval(d time.Duration) Option {
	return func(o *Options) {
		o.PollInterval = d
	}
}

// WithProcessingMode sets how messages are handled after successful relay.
// ProcessingModeDelete removes messages from pending table (default).
// ProcessingModeMove moves messages to completed table for audit trail.
func WithProcessingMode(mode ProcessingMode) Option {
	return func(o *Options) {
		o.ProcessingMode = mode
	}
}

// WithLogger sets the logger for relay errors.
func WithLogger(l Logger) Option {
	return func(o *Options) {
		o.Logger = l
	}
}

// WithErrorHandler sets a custom error handler for failed message sends.
// If not set, errors are logged and the relay continues with the next message.
func WithErrorHandler(handler func(ctx context.Context, msg *outbox.Message, err error)) Option {
	return func(o *Options) {
		o.ErrorHandler = handler
	}
}

// WithLeaderElection enables leader election for running multiple relay instances.
// Only one instance will be active at a time. Requires the store to implement LeaderStore.
// The lockName should be unique per relay group (e.g., "outbox-relay").
func WithLeaderElection(lockName string) Option {
	return func(o *Options) {
		o.LeaderLockName = lockName
	}
}

// WithLeaseTTL sets the leader lock lease duration.
// The lock expires after this duration if not renewed. Default is 30 seconds.
func WithLeaseTTL(ttl time.Duration) Option {
	return func(o *Options) {
		o.LeaseTTL = ttl
	}
}
