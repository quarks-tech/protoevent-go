package relay

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/quarks-tech/protoevent-go/pkg/eventbus"
	"github.com/quarks-tech/protoevent-go/pkg/transport/outbox"
)

// Store defines operations needed by the message relay.
// This is typically implemented by a non-transactional store instance.
//
// The relay uses a two-table approach:
//   - outbox_pending: messages waiting to be sent
//   - outbox_completed: messages that have been sent (optional, for audit)
//
// This eliminates the need for cursor management and prevents race conditions
// where messages with earlier UUIDs are inserted after later ones.
type Store interface {
	// ListPendingMessages retrieves messages from the pending table,
	// ordered by id (FIFO). Returns up to 'limit' messages.
	// Uses UUID v7 IDs which are time-sortable.
	ListPendingMessages(ctx context.Context, limit int) ([]*outbox.Message, error)

	// CompletePendingMessages marks messages as successfully sent.
	// The implementation decides what "complete" means:
	//   - Delete from pending table (no audit trail)
	//   - Move to completed table with sentTime (audit trail)
	//   - Any other completion strategy
	CompletePendingMessages(ctx context.Context, sentTime time.Time, ids ...string) error
}

// LeaderStore is an optional interface for stores that support leader election.
// Implement this interface to enable running multiple relay instances with automatic failover.
// Only one instance will be active at a time, ensuring strict FIFO ordering.
type LeaderStore interface {
	// TryAcquireLeaderLock attempts to acquire or renew the leader lock.
	// Returns true if this instance (identified by holderID) holds the lock after the call.
	// The lock expires after leaseTTL if not renewed.
	TryAcquireLeaderLock(ctx context.Context, name, holderID string, leaseTTL time.Duration) (bool, error)
}

// Logger interface for relay error logging.
type Logger interface {
	Errorf(format string, args ...any)
}

// Options contains configuration for relay instances.
type Options struct {
	BatchSize    int
	PollInterval time.Duration
	Logger       Logger
	ErrorHandler func(ctx context.Context, msg *outbox.Message, err error)

	// Leader election options
	LeaderLockName string
	LeaseTTL       time.Duration
}

// DefaultOptions returns the default relay configuration.
func DefaultOptions() Options {
	return Options{
		BatchSize:    100,
		PollInterval: time.Second,
		LeaseTTL:     30 * time.Second,
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

// Relay reads messages from the pending table and forwards them to another transport.
// It processes messages in FIFO order and marks them as completed after successful send.
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
