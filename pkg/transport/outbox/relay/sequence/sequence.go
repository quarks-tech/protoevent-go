// Package sequence is the TiDB sequenced-log outbox relay runtime: a leader runs
// a post-commit sequencer pass then drains the log in seq order to a Sender.
package sequence

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/quarks-tech/protoevent-go/pkg/eventbus"
	"github.com/quarks-tech/protoevent-go/pkg/transport/outbox"
	"github.com/quarks-tech/protoevent-go/pkg/transport/outbox/relay"
)

// Observer extends the shared relay.Observer with the sequence-specific
// sequencer-pass signal.
type Observer interface {
	relay.Observer
	// ObserveSequenced reports how many rows a sequencer pass assigned.
	ObserveSequenced(name string, count int)
}

type nopObserver struct{}

func (nopObserver) ObserveDrained(string, int, time.Duration, bool) {}
func (nopObserver) ObserveError(string, error)                      {}
func (nopObserver) ObserveSequenced(string, int)                    {}

// Store is the sequenced-log read/offset contract, implemented over a
// non-transactional connection (e.g. *sql.DB).
type Store interface {
	// ListMessages returns sequenced messages with Seq > afterSeq, ordered by
	// Seq ascending, up to limit. Unsequenced rows (Seq NULL) are excluded.
	ListMessages(ctx context.Context, afterSeq int64, limit int) ([]*outbox.Message, error)

	// Offset returns the last committed Seq for the named consumer (0 if none).
	Offset(ctx context.Context, name string) (int64, error)

	// CommitOffset advances the named consumer's watermark. Implementations MUST
	// be monotone (GREATEST semantics): a lower seq never rewinds the offset.
	CommitOffset(ctx context.Context, name string, seq int64) error

	// InitOffsetLatest is called once for a consumer group with no committed
	// offset: atomically initialize its offset row to the current maximum
	// assigned seq (0 if the log is empty or unsequenced) and return the
	// effective offset. Implementations MUST be monotone (GREATEST) so it never
	// rewinds an existing row.
	InitOffsetLatest(ctx context.Context, name string) (int64, error)
}

// SequencerStore assigns dense Seq values to committed-but-unsequenced rows.
// Implementations MUST serialize passes (counter row FOR UPDATE) and order the
// batch by (tx_start_ts, id). Returns the number of rows sequenced.
type SequencerStore interface {
	SequenceMessages(ctx context.Context, limit int) (int, error)
}

// RetentionStore prunes fully-consumed rows below every consumer's offset.
type RetentionStore interface {
	// SweepMessages deletes sequenced rows whose Seq is <= the minimum committed
	// offset across all consumers AND whose event time is before `before`,
	// bounded to `limit` rows. Returns the number deleted.
	SweepMessages(ctx context.Context, before time.Time, limit int) (int, error)
}

// Options contains configuration for a sequence relay instance.
type Options struct {
	BatchSize         int // drain page size (network sends)
	SequenceBatchSize int // sequencer page size (cheap UPDATE)
	PollInterval      time.Duration
	LeaseTTL          time.Duration
	LeaderLockName    string // defaults to the relay name

	DisableSequencer bool

	// StartFromBeginning makes a NEW consumer group replay the retained log
	// from the start instead of the default "latest" (future events only —
	// parity with the stream runtime's start-at-now). Has no effect once the
	// group has a committed offset.
	StartFromBeginning bool

	RetentionWindow     time.Duration // 0 disables the sweep
	RetentionSweepEvery int           // run sweep every N ticks
	RetentionSweepBatch int

	Logger       relay.Logger
	Observer     Observer
	ErrorHandler func(ctx context.Context, msg *outbox.Message, err error)
}

// DefaultOptions returns the default relay configuration.
func DefaultOptions() Options {
	return Options{
		BatchSize:         100,
		SequenceBatchSize: 1000,
		PollInterval:      time.Second,
		LeaseTTL:          15 * time.Second,
		Observer:          nopObserver{},
	}
}

// Option configures relay options.
type Option func(*Options)

func WithBatchSize(size int) Option           { return func(o *Options) { o.BatchSize = size } }
func WithSequenceBatchSize(size int) Option   { return func(o *Options) { o.SequenceBatchSize = size } }
func WithPollInterval(d time.Duration) Option { return func(o *Options) { o.PollInterval = d } }
func WithLeaseTTL(ttl time.Duration) Option   { return func(o *Options) { o.LeaseTTL = ttl } }
func WithLeaderLockName(name string) Option   { return func(o *Options) { o.LeaderLockName = name } }
func WithoutSequencer() Option                { return func(o *Options) { o.DisableSequencer = true } }
func WithLogger(l relay.Logger) Option        { return func(o *Options) { o.Logger = l } }

// WithStartFromBeginning makes a NEW consumer group (one with no committed
// offset) replay the retained log from the start instead of the default
// "latest" (future events only — parity with the stream runtime's
// start-at-now). Has no effect once the group has a committed offset.
func WithStartFromBeginning() Option {
	return func(o *Options) { o.StartFromBeginning = true }
}

// WithObserver sets the observability sink. A nil observer is ignored.
func WithObserver(obs Observer) Option {
	return func(o *Options) {
		if obs != nil {
			o.Observer = obs
		}
	}
}

// WithErrorHandler switches send-failure handling from stop-the-lane (default,
// order-preserving) to park-and-continue: the handler is called and the relay
// advances past the failed message. This trades per-event order for liveness.
func WithErrorHandler(h func(ctx context.Context, msg *outbox.Message, err error)) Option {
	return func(o *Options) { o.ErrorHandler = h }
}

// WithRetention enables the retention sweep on the leader.
func WithRetention(window time.Duration, sweepEvery, sweepBatch int) Option {
	return func(o *Options) {
		o.RetentionWindow = window
		o.RetentionSweepEvery = sweepEvery
		o.RetentionSweepBatch = sweepBatch
	}
}

// Relay tails the sequenced log for one consumer (identified by name) and
// forwards messages to another transport in Seq order.
type Relay struct {
	name    string
	store   Store
	sender  eventbus.Sender
	options Options
	leader  *relay.LeaderElector

	tickCount int // for retention cadence

	// offsetInitialized latches once InitOffsetLatest has run for this Relay
	// instance, so a still-empty-log group (offset genuinely 0 with nothing to
	// commit yet) doesn't re-run InitOffsetLatest on every drain() tick.
	offsetInitialized bool
}

// NewRelay creates a relay for the named consumer group.
//
// name identifies the consumer (its offset row) and is the default leader-lock
// name. store reads the log and holds offsets; sender is the downstream
// transport (e.g. a RabbitMQ sender). If store also implements SequencerStore,
// this relay runs the sequencer pass unless WithoutSequencer() is given.
func NewRelay(name string, store Store, sender eventbus.Sender, opts ...Option) *Relay {
	options := DefaultOptions()
	for _, opt := range opts {
		opt(&options)
	}
	if options.LeaderLockName == "" {
		options.LeaderLockName = name
	}

	return &Relay{
		name:    name,
		store:   store,
		sender:  sender,
		options: options,
		leader:  relay.NewLeaderElector(store, options.LeaderLockName, uuid.NewString(), options.LeaseTTL),
	}
}
