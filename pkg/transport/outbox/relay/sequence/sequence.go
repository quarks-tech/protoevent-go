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

// options contains configuration for a sequence relay instance. It is
// unexported on purpose: the only way to configure a relay is the With*
// option constructors, whose nil/zero guards are then the single validation
// surface — an exported mutable struct alongside ...Option would make invalid
// states representable and force defensive re-defaulting in NewRelay.
type options struct {
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

	RetentionWindow        time.Duration // 0 disables the sweep
	RetentionSweepInterval time.Duration // minimum time between sweeps
	RetentionSweepBatch    int

	Logger       *slog.Logger // defaults to a discard logger
	Observer     relay.Observer
	ErrorHandler relay.ErrorHandler
}

// defaultOptions returns the default relay configuration. The zero
// relay.Observer discards all signals.
func defaultOptions() options {
	return options{
		BatchSize:         100,
		SequenceBatchSize: 1000,
		PollInterval:      time.Second,
		LeaseTTL:          15 * time.Second,
		Logger:            slog.New(slog.DiscardHandler),
	}
}

// Option configures relay options.
type Option func(*options)

// WithBatchSize sets the drain page size (messages listed and sent per page).
func WithBatchSize(size int) Option { return func(o *options) { o.BatchSize = size } }

// WithSequenceBatchSize sets the sequencer page size (rows assigned per pass).
func WithSequenceBatchSize(size int) Option { return func(o *options) { o.SequenceBatchSize = size } }

// WithPollInterval sets the tick interval between relay passes.
func WithPollInterval(d time.Duration) Option { return func(o *options) { o.PollInterval = d } }

// WithLeaseTTL sets the leader-lease TTL.
func WithLeaseTTL(ttl time.Duration) Option { return func(o *options) { o.LeaseTTL = ttl } }

// WithLeaderLockName overrides the leader-lock name (defaults to the relay name).
func WithLeaderLockName(name string) Option { return func(o *options) { o.LeaderLockName = name } }

// WithoutSequencer disables this relay's sequencer pass. When several consumer
// groups share one store, run the sequencer in exactly one relay and configure
// the others with WithoutSequencer(): each extra relay would otherwise run a
// redundant sequencer pass every tick — correctness is unaffected (passes
// serialize on the counter row), but the serialized DB work is wasted.
func WithoutSequencer() Option { return func(o *options) { o.SequencerDisabled = true } }

// WithLogger sets the error logger. A nil logger is ignored.
func WithLogger(l *slog.Logger) Option {
	return func(o *options) {
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
	return func(o *options) { o.StartFromBeginning = true }
}

// WithObserver sets the observability sink (a relay.Observer struct of
// nil-able callbacks; both runtimes accept the same type — OnSequenced fires
// only here). The zero value discards all signals.
func WithObserver(obs relay.Observer) Option {
	return func(o *options) { o.Observer = obs }
}

// WithErrorHandler installs the poison-parking hook: a row whose PERSISTED
// METADATA fails to decode (*DecodeError) is handed to h and the relay
// advances past it — retrying a poison row can never succeed, so parking it is
// the only way to keep the lane moving. Send failures are NOT parked
// regardless of this option: a send failure is downstream trouble, and the
// lane always stops and retries the same message next tick (order and
// delivery preserved). Without an ErrorHandler a poison row stops the lane.
func WithErrorHandler(h relay.ErrorHandler) Option {
	return func(o *options) { o.ErrorHandler = h }
}

// WithRetention enables the retention sweep on the leader: at most one sweep
// per sweepInterval, deleting up to sweepBatch fully-consumed rows older than
// window per sweep. sweepInterval is wall-clock time, decoupled from
// PollInterval — retuning the tick does not silently change sweep cadence.
func WithRetention(window, sweepInterval time.Duration, sweepBatch int) Option {
	return func(o *options) {
		o.RetentionWindow = window
		o.RetentionSweepInterval = sweepInterval
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
	options options
	leader  *relay.LeaderElector

	sequencer SequencerStore // nil if store lacks the capability or WithoutSequencer
	retention RetentionStore // nil if store lacks the capability or retention not configured

	lastSweep time.Time // for retention cadence (see maybeSweep)

	// offsetInitialized latches once InitOffsetLatest has run for this Relay
	// instance. With insert-if-absent semantics a re-run is harmless (an
	// existing row is never modified), so the latch is only a round-trip
	// optimization: it keeps a still-empty-log group (offset genuinely 0 with
	// nothing to commit yet) from re-running InitOffsetLatest on every
	// drain() tick.
	offsetInitialized bool
}

// maxNameLen bounds the consumer-group and leader-lock names. The reference
// schema (migrations/) keys outbox_offsets, relay_locks, and outbox_sequencers
// on VARCHAR(64): under strict sql_mode a longer name fails every tick with a
// generic 1406 far from the misconfiguration, and under a relaxed sql_mode it
// is SILENTLY TRUNCATED — two groups sharing a 64-char prefix would then share
// one offset row and one leader lock (cross-group event loss + broken mutual
// exclusion). Rejected at construction instead.
const maxNameLen = 64

// NewRelay creates a relay for the named consumer group. It validates the
// configuration and returns an error if:
//
//   - name is empty: name is the offset-row key and the default leader-lock
//     name, so two empty-named relays would silently share both;
//   - name (or an overridden LeaderLockName) exceeds 64 bytes: the reference
//     schema's key columns are VARCHAR(64), see maxNameLen;
//   - store or sender is nil;
//   - PollInterval, BatchSize, SequenceBatchSize, or LeaseTTL is not strictly
//     positive: a zero PollInterval panics inside time.NewTicker, and a zero
//     BatchSize or SequenceBatchSize would silently stall the relay;
//   - PollInterval is not strictly less than LeaseTTL/2: the lease is renewed
//     once per tick (and between pages), so it must be renewable at least
//     twice per TTL or it expires between renewals and leadership silently
//     flaps every tick (mirrors the stream runtime's DrainWindow guard);
//   - RetentionWindow is set without a positive sweep interval and batch;
//   - store lacks SequencerStore and WithoutSequencer() was not given: without
//     a sequencer nothing ever gets a seq and the relay silently delivers
//     nothing — the legitimate someone-else-sequences topology has an explicit
//     spelling;
//   - WithRetention is set but store lacks RetentionStore: a configured sweep
//     that silently never runs grows the log unboundedly.
//
// name identifies the consumer (its offset row) and is the default leader-lock
// name. store reads the log and holds offsets; sender is the downstream
// transport (e.g. a RabbitMQ sender). When several consumer groups share one
// store, run the sequencer in exactly one relay and configure the others with
// WithoutSequencer() — see its doc.
func NewRelay(name string, store Store, sender eventbus.Sender, opts ...Option) (*Relay, error) {
	if name == "" {
		return nil, errors.New("sequence: name must not be empty (it is the offset-row key and the default leader-lock name)")
	}
	if len(name) > maxNameLen {
		return nil, fmt.Errorf("sequence: name %q exceeds %d bytes (the reference schema's VARCHAR(64) key columns; a relaxed sql_mode would silently truncate it into another group's offset row and leader lock)", name, maxNameLen)
	}
	if store == nil {
		return nil, errors.New("sequence: store must not be nil")
	}
	if sender == nil {
		return nil, errors.New("sequence: sender must not be nil")
	}

	options := defaultOptions()
	for _, opt := range opts {
		opt(&options)
	}
	if options.LeaderLockName == "" {
		options.LeaderLockName = name
	}
	if len(options.LeaderLockName) > maxNameLen {
		return nil, fmt.Errorf("sequence: LeaderLockName %q exceeds %d bytes (the reference schema's VARCHAR(64) key column)", options.LeaderLockName, maxNameLen)
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
	if options.RetentionWindow > 0 && (options.RetentionSweepInterval <= 0 || options.RetentionSweepBatch <= 0) {
		return nil, fmt.Errorf("sequence: RetentionWindow (%v) requires RetentionSweepInterval > 0 and RetentionSweepBatch > 0, got %v and %d",
			options.RetentionWindow, options.RetentionSweepInterval, options.RetentionSweepBatch)
	}

	ls, hasLeaderStore := store.(relay.LeaderStore)
	if !hasLeaderStore {
		// Always-leader is a documented single-instance mode, not an error —
		// but it must not be a silent one: a custom store that MEANT to
		// implement relay.LeaderStore (wrong signature, broken refactor)
		// would otherwise run dual leaders in a multi-replica deployment
		// with no signal until duplicate delivery is noticed.
		options.Logger.Info("sequence relay: store has no relay.LeaderStore capability; running always-leader (single-instance mode)",
			"relay", name)
	}

	r := &Relay{
		name: name,
		// Every store capability is decorated with the LeaseTTL operation
		// bound at construction — see boundedStore (bounded.go).
		store:   boundedStore{inner: store, ttl: options.LeaseTTL},
		sender:  sender,
		options: options,
		leader:  relay.NewLeaderElector(ls, options.LeaderLockName, uuid.NewString(), options.LeaseTTL),
	}

	// Capability implied by configuration must exist unless explicitly waived:
	// a silently missing capability is an outage, not a mode. Without a
	// sequencer (and no WithoutSequencer waiver) rows stay unsequenced and the
	// relay delivers NOTHING while reporting no error — a permanent silent
	// stall. A configured retention window without a RetentionStore never
	// sweeps — unbounded table growth surfacing as a disk incident long after
	// the misconfiguration. (The LeaderStore assertion above stays soft:
	// always-leader is a documented single-instance mode, not a configured
	// capability.)
	if !options.SequencerDisabled {
		seq, ok := store.(SequencerStore)
		if !ok {
			return nil, errors.New("sequence: store does not implement sequence.SequencerStore; " +
				"pass WithoutSequencer() if another relay runs the sequencer for this store")
		}
		r.sequencer = boundedSequencer{inner: seq, ttl: options.LeaseTTL}
	}
	if options.RetentionWindow > 0 {
		ret, ok := store.(RetentionStore)
		if !ok {
			return nil, errors.New("sequence: WithRetention is set but store does not implement sequence.RetentionStore")
		}
		r.retention = boundedRetention{inner: ret, ttl: options.LeaseTTL}
	}

	return r, nil
}
