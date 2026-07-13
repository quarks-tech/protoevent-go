// Package sequence is the TiDB sequenced-log outbox relay runtime: a leader runs
// a post-commit sequencer pass then drains the log in seq order to a Sender.
package sequence

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
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

// nopObserver extends relay.NopObserver with a no-op ObserveSequenced, so the
// default observer satisfies this package's extended Observer without
// reimplementing the shared signals.
type nopObserver struct{ relay.Observer }

func (nopObserver) ObserveSequenced(string, int) {}

func newNopObserver() Observer { return nopObserver{relay.NopObserver()} }

// DecodeError reports a row whose persisted metadata failed to decode. The
// store returns it from ListMessages together with the successfully decoded
// prefix of the page; a relay with an ErrorHandler parks the row and advances
// past Seq, otherwise the lane stops (at-least-once, order preserved).
type DecodeError struct {
	ID  string // event_id of the poison row
	Seq int64  // its assigned seq
	Err error
}

func (e *DecodeError) Error() string {
	return fmt.Sprintf("sequence: decode message %s (seq %d): %v", e.ID, e.Seq, e.Err)
}

func (e *DecodeError) Unwrap() error { return e.Err }

// Store is the sequenced-log read/offset contract, implemented over a
// non-transactional connection (e.g. *sql.DB).
type Store interface {
	// ListMessages returns sequenced messages with Seq > afterSeq, ordered by
	// Seq ascending, up to limit. Unsequenced rows (Seq NULL) are excluded.
	// If a row's persisted metadata fails to decode, ListMessages returns the
	// successfully decoded prefix of the page together with a *DecodeError
	// identifying the poison row.
	ListMessages(ctx context.Context, afterSeq int64, limit int) ([]*outbox.Message, error)

	// Offset returns the last committed Seq for the named consumer (0 if none).
	Offset(ctx context.Context, name string) (int64, error)

	// CommitOffset advances the named consumer's watermark. Implementations MUST
	// be monotone (GREATEST semantics): a lower seq never rewinds the offset.
	CommitOffset(ctx context.Context, name string, seq int64) error

	// InitOffsetLatest is called when the consumer group has no
	// in-memory-confirmed offset. Implementations MUST be insert-if-absent:
	// create the offset row at the current maximum assigned seq (0 if the log
	// is empty) ONLY if no row exists, and return the effective committed
	// offset. An existing row — even at 0 — is a committed position and MUST
	// NOT be modified: forward-jumping it skips events.
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
	// offset across all consumers AND whose insert time (CreateTime) is before
	// `before`, bounded to `limit` rows. Returns the number deleted.
	SweepMessages(ctx context.Context, before time.Time, limit int) (int, error)
}

// Options contains configuration for a sequence relay instance.
type Options struct {
	BatchSize         int // drain page size (network sends)
	SequenceBatchSize int // sequencer page size (cheap UPDATE)
	PollInterval      time.Duration
	LeaseTTL          time.Duration
	LeaderLockName    string // defaults to the relay name

	SequencerDisabled bool // disables this relay's sequencer pass (see WithoutSequencer)

	// StartFromBeginning makes a NEW consumer group replay the retained log
	// from the start instead of the default "latest" (future events only —
	// parity with the stream runtime's start-at-now). Has no effect once the
	// group has a committed offset.
	StartFromBeginning bool

	RetentionWindow     time.Duration // 0 disables the sweep
	RetentionSweepEvery int           // run sweep every N ticks
	RetentionSweepBatch int

	Logger       *slog.Logger // defaults to a discard logger
	Observer     Observer
	ErrorHandler relay.ErrorHandler
}

// DefaultOptions returns the default relay configuration.
func DefaultOptions() Options {
	return Options{
		BatchSize:         100,
		SequenceBatchSize: 1000,
		PollInterval:      time.Second,
		LeaseTTL:          15 * time.Second,
		Observer:          newNopObserver(),
		Logger:            slog.New(slog.DiscardHandler),
	}
}

// Option configures relay options.
type Option func(*Options)

// WithBatchSize sets the drain page size (messages listed and sent per page).
func WithBatchSize(size int) Option { return func(o *Options) { o.BatchSize = size } }

// WithSequenceBatchSize sets the sequencer page size (rows assigned per pass).
func WithSequenceBatchSize(size int) Option { return func(o *Options) { o.SequenceBatchSize = size } }

// WithPollInterval sets the tick interval between relay passes.
func WithPollInterval(d time.Duration) Option { return func(o *Options) { o.PollInterval = d } }

// WithLeaseTTL sets the leader-lease TTL.
func WithLeaseTTL(ttl time.Duration) Option { return func(o *Options) { o.LeaseTTL = ttl } }

// WithLeaderLockName overrides the leader-lock name (defaults to the relay name).
func WithLeaderLockName(name string) Option { return func(o *Options) { o.LeaderLockName = name } }

// WithoutSequencer disables this relay's sequencer pass. When several consumer
// groups share one store, run the sequencer in exactly one relay and configure
// the others with WithoutSequencer(): each extra relay would otherwise run a
// redundant sequencer pass every tick — correctness is unaffected (passes
// serialize on the counter row), but the serialized DB work is wasted.
func WithoutSequencer() Option { return func(o *Options) { o.SequencerDisabled = true } }

// WithLogger sets the error logger. A nil logger is ignored.
func WithLogger(l *slog.Logger) Option {
	return func(o *Options) {
		if l != nil {
			o.Logger = l
		}
	}
}

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
func WithErrorHandler(h relay.ErrorHandler) Option {
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
//
// A Relay is not safe for concurrent use: Run (or RunOnce) must be called from
// a single goroutine.
type Relay struct {
	name    string
	store   Store
	sender  eventbus.Sender
	options Options
	leader  *relay.LeaderElector

	sequencer SequencerStore // nil if store lacks the capability or WithoutSequencer
	retention RetentionStore // nil if store lacks the capability or retention not configured

	tickCount int // for retention cadence

	// offsetInitialized latches once InitOffsetLatest has run for this Relay
	// instance. With insert-if-absent semantics a re-run is harmless (an
	// existing row is never modified), so the latch is only a round-trip
	// optimization: it keeps a still-empty-log group (offset genuinely 0 with
	// nothing to commit yet) from re-running InitOffsetLatest on every
	// drain() tick.
	offsetInitialized bool
}

// NewRelay creates a relay for the named consumer group. It validates the
// configuration and returns an error if:
//
//   - name is empty: name is the offset-row key and the default leader-lock
//     name, so two empty-named relays would silently share both;
//   - store or sender is nil;
//   - PollInterval, BatchSize, SequenceBatchSize, or LeaseTTL is not strictly
//     positive: a zero PollInterval panics inside time.NewTicker, and a zero
//     BatchSize or SequenceBatchSize would silently stall the relay;
//   - PollInterval is not strictly less than LeaseTTL/2: the lease is renewed
//     once per tick (and between pages), so it must be renewable at least
//     twice per TTL or it expires between renewals and leadership silently
//     flaps every tick (mirrors the stream runtime's DrainWindow guard);
//   - RetentionWindow is set without a positive sweep cadence and batch.
//
// name identifies the consumer (its offset row) and is the default leader-lock
// name. store reads the log and holds offsets; sender is the downstream
// transport (e.g. a RabbitMQ sender). If store also implements SequencerStore,
// this relay runs the sequencer pass unless WithoutSequencer() is given. When
// several consumer groups share one store, run the sequencer in exactly one
// relay and configure the others with WithoutSequencer() — see its doc.
func NewRelay(name string, store Store, sender eventbus.Sender, opts ...Option) (*Relay, error) {
	if name == "" {
		return nil, errors.New("sequence: name must not be empty (it is the offset-row key and the default leader-lock name)")
	}
	if store == nil {
		return nil, errors.New("sequence: store must not be nil")
	}
	if sender == nil {
		return nil, errors.New("sequence: sender must not be nil")
	}

	options := DefaultOptions()
	for _, opt := range opts {
		opt(&options)
	}
	// A raw Option (not the With* helpers, which guard against nil) can nil
	// these out; re-default so the runtime never nil-derefs on its hot path.
	if options.Observer == nil {
		options.Observer = newNopObserver()
	}
	if options.Logger == nil {
		options.Logger = slog.New(slog.DiscardHandler)
	}
	if options.LeaderLockName == "" {
		options.LeaderLockName = name
	}
	if options.PollInterval <= 0 {
		return nil, fmt.Errorf("sequence: PollInterval must be > 0, got %v", options.PollInterval)
	}
	if options.BatchSize <= 0 {
		return nil, fmt.Errorf("sequence: BatchSize must be > 0, got %d", options.BatchSize)
	}
	if options.SequenceBatchSize <= 0 {
		return nil, fmt.Errorf("sequence: SequenceBatchSize must be > 0, got %d", options.SequenceBatchSize)
	}
	if options.LeaseTTL <= 0 {
		return nil, fmt.Errorf("sequence: LeaseTTL must be > 0, got %v", options.LeaseTTL)
	}
	if options.PollInterval >= options.LeaseTTL/2 {
		return nil, fmt.Errorf("sequence: PollInterval (%v) must be < LeaseTTL/2 (%v)", options.PollInterval, options.LeaseTTL/2)
	}
	if options.RetentionWindow > 0 && (options.RetentionSweepEvery <= 0 || options.RetentionSweepBatch <= 0) {
		return nil, fmt.Errorf("sequence: RetentionWindow (%v) requires RetentionSweepEvery > 0 and RetentionSweepBatch > 0, got %d and %d",
			options.RetentionWindow, options.RetentionSweepEvery, options.RetentionSweepBatch)
	}

	ls, _ := store.(relay.LeaderStore)

	r := &Relay{
		name:    name,
		store:   store,
		sender:  sender,
		options: options,
		leader:  relay.NewLeaderElector(ls, options.LeaderLockName, uuid.NewString(), options.LeaseTTL),
	}

	if !options.SequencerDisabled {
		r.sequencer, _ = store.(SequencerStore)
	}
	if options.RetentionWindow > 0 {
		r.retention, _ = store.(RetentionStore)
	}

	return r, nil
}
