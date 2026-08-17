// Package sequence is the TiDB sequenced-log outbox relay runtime: a leader runs
// a post-commit sequencer pass then drains the log in seq order to a Sender.
//
// Two audiences use this package. END USERS construct a Relay (NewRelay +
// With* options, typically over tidb.NewRelayStore) and call Run; RunOnce
// exists for tests and custom drivers. STORE AUTHORS implement the Store /
// Sequencer / Sweeper contracts (plus relay.LeaderStore) for a
// new backend — the tidb module is the reference implementation.
package sequence

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/quarks-tech/protoevent-go/pkg/eventbus"
	"github.com/quarks-tech/protoevent-go/pkg/transport/outbox"
	"github.com/quarks-tech/protoevent-go/pkg/transport/outbox/internal/lane"
	"github.com/quarks-tech/protoevent-go/pkg/transport/outbox/internal/leader"
	"github.com/quarks-tech/protoevent-go/pkg/transport/outbox/internal/notify"
	"github.com/quarks-tech/protoevent-go/pkg/transport/outbox/internal/relaycfg"
	"github.com/quarks-tech/protoevent-go/pkg/transport/outbox/relay"
)

// DecodeError reports a row whose persisted metadata failed to decode. The
// store returns it from ListMessages together with the successfully decoded
// prefix of the page; a relay with a PoisonHandler parks the row and advances
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

// StuckLaneError is relay.StuckLaneError, re-exported so callers of this package
// need not import relay to match on it. See its doc for the contract; Position
// carries "seq <n>" for this runtime.
type StuckLaneError = relay.StuckLaneError

// Store is the sequenced-log read/offset contract, implemented over a
// non-transactional connection (e.g. *sql.DB).
type Store interface {
	// ListMessages returns sequenced messages with Seq > afterSeq, ordered by
	// Seq ascending, up to limit. Unsequenced rows (Seq NULL) are excluded.
	// If a row's persisted metadata fails to decode, ListMessages returns the
	// successfully decoded prefix of the page together with a *DecodeError
	// identifying the poison row.
	ListMessages(ctx context.Context, afterSeq int64, limit int) ([]*outbox.Message, error)

	// Offset returns the last committed Seq for the named consumer, and whether an
	// offset row exists at all.
	//
	// exists is not cosmetic: a group's committed offset is legitimately 0 (a fresh
	// group, or one waiting on the sequencer), so a bare 0 cannot distinguish
	// "registered here" from "no row". Collapsing the two forces the relay either to
	// re-prime on every single tick — three round trips, one of them a write or a
	// deliberate duplicate-key failure, once per PollInterval forever — or to cache
	// "primed" in memory, which turns a deleted offset row (the documented
	// DeleteOffset decommission, applied to a running group) into a full replay of
	// the retained log. With exists the relay primes exactly when there is no row.
	Offset(ctx context.Context, name string) (offset int64, exists bool, err error)

	// CommitOffset advances the named consumer's watermark. Implementations MUST
	// be monotone (GREATEST semantics): a lower seq never rewinds the offset.
	// They MUST also be insert-if-absent: the relay registers a
	// WithStartFromBeginning group by committing seq 0 before its first read, so
	// that the retention sweep's MIN(last_seq) cutoff accounts for the group
	// from the start. An UPDATE-only implementation would leave the group
	// unregistered and let the sweep delete the history it is replaying.
	CommitOffset(ctx context.Context, name string, seq int64) error

	// InitOffsetLatest is called when the consumer group has no
	// in-memory-confirmed offset. Implementations MUST be insert-if-absent:
	// create the offset row at the current maximum assigned seq (0 if the log
	// is empty) ONLY if no row exists, and return the effective committed
	// offset. An existing row — even at 0 — is a committed position and MUST
	// NOT be modified: forward-jumping it skips events.
	//
	// "Latest" means max ASSIGNED seq: committed-but-unsequenced rows are not
	// counted, so a group primed before the sequencer catches up will receive
	// that backlog once it is sequenced — see WithoutSequencer's start-order
	// caveat.
	InitOffsetLatest(ctx context.Context, name string) (int64, error)
}

// Sequencer assigns dense Seq values to committed-but-unsequenced rows.
// Implementations MUST serialize passes (counter row FOR UPDATE) and order the
// batch by (tx_start_ts, id). Returns the number of rows sequenced.
type Sequencer interface {
	SequenceMessages(ctx context.Context, limit int) (int, error)
}

// Clock is an optional store capability supplying the store's own current time.
//
// It exists for one reason: lag. Message.CreateTime is stamped by the STORE
// (the DB's NOW at insert), so measuring its age against the relay host's
// time.Now() folds any NTP skew between the relay pod and the database into the
// reported lag — on a pod whose clock trails the DB, a genuinely stale backlog
// reports a negative age, which a Prometheus gauge plots as ~0 and no alert ever
// fires on. With this capability both operands come from the store's clock.
//
// A store that cannot cheaply answer it simply doesn't implement it: the relay
// then falls back to the host clock (see Relay.oldestAge). The relay calls
// StoreNow at most once per drain pass, and only when an Observer actually
// consumes the value, so an unobserved relay issues no extra query.
type Clock interface {
	StoreNow(ctx context.Context) (time.Time, error)
}

// Sweeper prunes fully-consumed rows below every consumer's offset.
type Sweeper interface {
	// SweepMessages deletes sequenced rows whose Seq is <= the minimum committed
	// offset across all consumers AND whose insert time is older than the
	// store's own retention window, bounded to `limit` rows. Returns the number
	// deleted.
	//
	// The WINDOW belongs to the store, not to this call: the sweep's cutoff is
	// MIN(last_seq) across ALL consumer groups, so its effect is store-wide
	// while a relay is per-group. Taking the window as a parameter made every
	// relay over one store declare a policy for all of them, and the shortest
	// window silently won — a second consumer group could truncate the first's
	// 30-day history to a 7-day default, diagnosable only by comparing two
	// startup logs. Configured on the store, the window is a property of the
	// log, which is what it always was.
	//
	// The cutoff MUST be evaluated against the STORE's clock (insert-time
	// column vs the database's NOW), never the relay host's wall clock: a
	// skewed relay host must not be able to sweep early or pin rows forever.
	SweepMessages(ctx context.Context, limit int) (int, error)
}

// options contains configuration for a sequence relay instance. It is
// unexported on purpose: the only way to configure a relay is the With*
// option constructors, whose nil/zero guards are then the single validation
// surface — an exported mutable struct alongside ...Option would make invalid
// states representable and force defensive re-defaulting in NewRelay.
type options struct {
	// Lease, Ops and Hooks are what both runtimes configure the same way; the
	// With* constructors below are one-liners over their promoted fields.
	relaycfg.Lease
	relaycfg.Ops
	relaycfg.Hooks

	BatchSize         int // drain page size (network sends)
	SequenceBatchSize int // sequencer page size (cheap UPDATE)
	PollInterval      time.Duration

	SequencerDisabled bool // disables this relay's sequencer pass (see WithoutSequencer)

	// StartFromBeginning makes a NEW consumer group replay the retained log
	// from the start instead of the default "latest" (future events only —
	// parity with the stream runtime's start-at-now). Has no effect once the
	// group has a committed offset.
	StartFromBeginning bool

	RetentionConfigured    bool          // WithRetention was called (requires the Sweeper capability)
	RetentionDisabled      bool          // WithoutRetention was called (this relay runs no sweep)
	RetentionSweepInterval time.Duration // minimum time between sweeps
	RetentionSweepBatch    int
}

// Default sweep CADENCE — how often this relay asks the store to prune, and how
// many rows per pass. The retention WINDOW is the store's (see Sweeper).
//
// Sweeping is on by default because the v2 log is never pruned on delivery — a
// delivered row stays so other consumer groups can still read it — so a store
// nobody sweeps grows until the cluster runs out of disk, months after the
// omission and nowhere near it. Defaulting is safe for DELIVERY: the sweep only
// ever deletes rows below EVERY group's committed offset (MIN(last_seq)) that are
// also older than the store's window, so it cannot touch an undelivered event.
//
// On a store with several relays, sweeping from more than one is harmless but
// redundant (the passes just find less to do); WithoutRetention lets the others
// opt out. What used to be an operator rule — "every relay over one store must
// agree on the window" — no longer exists, because no relay carries a window.
const (
	defaultRetentionSweepInterval = time.Hour
	defaultRetentionSweepBatch    = 1000
)

// defaultOptions returns the default relay configuration. The shared defaults
// come from relaycfg — see DefaultHooks for why the default logger is
// slog.Default() and not a discard handler.
func defaultOptions() options {
	return options{
		Lease:                  relaycfg.DefaultLease(),
		Ops:                    relaycfg.DefaultOps(),
		Hooks:                  relaycfg.DefaultHooks(),
		BatchSize:              100,
		SequenceBatchSize:      1000,
		PollInterval:           time.Second,
		RetentionSweepInterval: defaultRetentionSweepInterval,
		RetentionSweepBatch:    defaultRetentionSweepBatch,
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

// WithLeaseTTL sets the leader-lease TTL — how long an ungraceful leader loss
// stalls the relay. It does not bound store calls; see WithOpTimeout.
func WithLeaseTTL(ttl time.Duration) Option { return func(o *options) { o.LeaseTTL = ttl } }

// WithOpTimeout sets the bound on every individual store call — the list, the
// offset commit, the sequencer pass, the sweep. It must exceed PollInterval.
func WithOpTimeout(d time.Duration) Option { return func(o *options) { o.OpTimeout = d } }

// WithLeaderLockName overrides the leader-lock name (defaults to the relay name).
func WithLeaderLockName(name string) Option { return func(o *options) { o.LeaderLockName = name } }

// WithoutLeaderElection declares a single-instance deployment: the relay always
// considers itself leader and never touches the store's lock, whether or not the
// store could elect.
//
// Without it, a store that does not implement relay.LeaderStore is a construction
// error. That is deliberate — leadership is the one capability whose silent
// absence is not a degraded mode but duplicate delivery, since every replica would
// forward the entire log. Run more than one replica with this option and you get
// exactly that.
func WithoutLeaderElection() Option {
	return func(o *options) { o.LeaderElectionDisabled = true }
}

// WithoutSequencer disables this relay's sequencer pass. When several consumer
// groups share one store, run the sequencer in exactly one relay and configure
// the others with WithoutSequencer(): each extra relay would otherwise run a
// redundant sequencer pass every tick — correctness is unaffected (passes
// serialize on the counter row), but the serialized DB work is wasted.
//
// START-ORDER CAVEAT: a NEW WithoutSequencer group's "latest" default is
// computed from MAX(assigned seq) — committed-but-not-yet-sequenced rows are
// invisible to it. If such a group primes its offset BEFORE the sequencing
// relay has caught up, the backlog those rows form is later sequenced ABOVE
// the primed offset and gets delivered — "latest" silently degrades into a
// partial replay. Start (or restart) WithoutSequencer groups only once the
// sequencing relay is running and caught up, or accept the replay (consumers
// dedup on Metadata.ID regardless).
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
//
// The group registers its offset row at seq 0 before its first read, so the
// retention sweep — whose cutoff is MIN(last_seq) across all groups — cannot
// delete the history it is about to replay. The flip side is that it PINS that
// cutoff at 0 store-wide until the group commits, so nothing is pruned while it
// catches up; OnSwept reports 0 throughout, which is the signal to watch.
func WithStartFromBeginning() Option {
	return func(o *options) { o.StartFromBeginning = true }
}

// WithObserver sets the observability sink (a relay.Observer struct of
// nil-able callbacks; both runtimes accept the same type — OnSequenced fires
// only here). The zero value discards all signals.
func WithObserver(obs relay.Observer) Option {
	return func(o *options) { o.Observer = obs }
}

// WithPoisonHandler installs the poison-parking hook: a row whose PERSISTED
// METADATA fails to decode (*DecodeError) is handed to h and the relay
// advances past it — retrying a poison row can never succeed, so parking it is
// the only way to keep the lane moving. Send failures are not parked by default:
// a send failure is downstream trouble, and the lane stops and retries the same
// message next tick (order and delivery preserved) — unless an
// UnsendableClassifier claims it, see WithUnsendableClassifier. Without a
// PoisonHandler a poison row stops the lane.
func WithPoisonHandler(h relay.PoisonHandler) Option {
	return func(o *options) { o.PoisonHandler = h }
}

// WithUnsendableClassifier installs the escape hatch for a message the
// downstream transport will never accept: when f reports a send failure
// permanent for that specific message, it is parked through the PoisonHandler
// and the lane advances past it instead of retrying it at the head of the log
// forever. Every failure f does not claim still stops the lane. Requires
// WithPoisonHandler. See relay.UnsendableClassifier for the (narrow) contract f
// must honor.
func WithUnsendableClassifier(f relay.UnsendableClassifier) Option {
	return func(o *options) { o.Unsendable = f }
}

// WithoutRetention stops THIS relay from running the retention sweep, which is
// otherwise on by default (the sequenced log is never pruned on delivery, so a
// store nobody sweeps grows until the cluster runs out of disk).
//
// Use it when something else owns pruning: another relay over the same store runs
// the sweep, the store prunes on its own, or an out-of-band DBA job does.
// Mutually exclusive with WithRetention.
//
// The retention WINDOW is configured on the store (see Sweeper), so this option
// no longer decides how much history survives — only whether this relay is one of
// the instances asking the store to prune.
func WithoutRetention() Option { return func(o *options) { o.RetentionDisabled = true } }

// WithRetention retunes this relay's sweep CADENCE: at most one sweep per
// sweepInterval, deleting up to sweepBatch fully-consumed rows per sweep.
// sweepInterval is wall-clock time, decoupled from PollInterval — retuning the
// tick does not silently change sweep cadence.
//
// How much history the sweep keeps is the store's retention window, not this
// relay's (see Sweeper). Calling this makes the Sweeper capability mandatory — an
// explicitly configured sweep that silently never runs is a disk incident in
// waiting. Use WithoutRetention to turn this relay's sweep off instead.
func WithRetention(sweepInterval time.Duration, sweepBatch int) Option {
	return func(o *options) {
		o.RetentionConfigured = true
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
	options options
	leader  *leader.Elector

	// reporter dispatches every observer/log signal; lane owns the per-message
	// send/park/stop policy and the stuck-lane escalation, both shared with the
	// stream runtime (internal/notify, internal/lane).
	reporter *notify.Reporter
	lane     *lane.Lane[int64]

	sequencer Sequencer // nil if store lacks the capability or WithoutSequencer
	retention Sweeper   // nil if store lacks the capability or retention not configured
	clock     Clock     // nil if store lacks the capability (lag falls back to the host clock)

	lastSweep time.Time // for retention cadence (see maybeSweep)

	// passStoreNow caches the store's clock for the current drain pass, so a
	// multi-page pass reads it once (see oldestAge). Reset at the top of drain.
	passStoreNow time.Time
}

// NewRelay creates a relay for the named consumer group. It validates the
// configuration and returns an error if:
//
//   - name is empty, or name (or an overridden LeaderLockName) exceeds
//     relaycfg.MaxNameLen bytes;
//   - store or sender is nil;
//   - PollInterval, BatchSize, SequenceBatchSize, LeaseTTL, or OpTimeout is not
//     strictly positive, or PollInterval is not strictly less than either
//     LeaseTTL/2 or OpTimeout;
//   - the sweep cadence (interval, batch) is not positive and WithoutRetention()
//     was not given, or WithRetention and WithoutRetention are both given;
//   - WithUnsendableClassifier is given without WithPoisonHandler;
//   - the store lacks a capability whose absence was not declared: no
//     relay.LeaderStore without WithoutLeaderElection(), no Sequencer without
//     WithoutSequencer(), or no Sweeper under an explicit WithRetention.
//
// Each rule's rationale is on the option that carries it; the common thread is
// that a capability implied by configuration must exist unless waived, because
// every silent absence here is an outage rather than a mode (no election ⇒ every
// replica delivers the whole log; no sequencer ⇒ nothing is ever assigned a seq
// and the relay delivers nothing while reporting no error; no sweep ⇒ the log
// grows until the disk does). A store lacking Sweeper under the DEFAULT cadence
// is the one soft case: the sweep is disabled with an Info log.
//
// name identifies the consumer (its offset row) and is the default leader-lock
// name. store reads the log and holds offsets; sender is the downstream
// transport (e.g. a RabbitMQ sender). When several consumer groups share one
// store, run the sequencer in exactly one relay and configure the others with
// WithoutSequencer() — see its doc.
//
// Sizing note (mirrors the stream runtime's TokenBatchSize rule): a drain
// page is up to BatchSize synchronous Sender.Send calls, and the lease is
// renewed only BETWEEN pages — so size BatchSize x worst-case Send latency
// < LeaseTTL, or a slow downstream can let one page outlive the lease and a
// transient second leader overlap it (at-least-once still holds; the
// single-active-consumer property weakens).
func NewRelay(name string, store Store, sender eventbus.Sender, opts ...Option) (*Relay, error) {
	if err := relaycfg.ValidateName("sequence", name); err != nil {
		return nil, err
	}
	if err := relaycfg.ValidateDeps("sequence", store, sender); err != nil {
		return nil, err
	}

	options := defaultOptions()
	for _, opt := range opts {
		opt(&options)
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
	if err := options.Lease.Validate("sequence", name, "PollInterval", options.PollInterval); err != nil {
		return nil, err
	}
	if err := options.Ops.Validate("sequence", "PollInterval", options.PollInterval); err != nil {
		return nil, err
	}
	if err := options.Hooks.Validate("sequence"); err != nil {
		return nil, err
	}
	if options.RetentionConfigured && options.RetentionDisabled {
		return nil, errors.New("sequence: WithRetention and WithoutRetention are mutually exclusive")
	}
	if !options.RetentionDisabled && (options.RetentionSweepInterval <= 0 || options.RetentionSweepBatch <= 0) {
		return nil, fmt.Errorf("sequence: the retention sweep requires RetentionSweepInterval > 0 and RetentionSweepBatch > 0, got %v and %d (pass WithoutRetention() to run no sweep)",
			options.RetentionSweepInterval, options.RetentionSweepBatch)
	}

	// Leadership is discovered by type assertion and its absence must be declared
	// (WithoutLeaderElection), never inferred: a silently unelected relay run on
	// several replicas delivers the whole log from every one of them.
	elector, err := options.Elector("sequence", store)
	if err != nil {
		return nil, err
	}

	reporter := options.Reporter("sequence", name)
	r := &Relay{
		name: name,
		// Every store capability is decorated with the OpTimeout operation
		// bound at construction — see boundedStore (bounded.go).
		store:    boundedStore{inner: store, ttl: options.OpTimeout},
		options:  options,
		leader:   elector,
		reporter: reporter,
		lane: &lane.Lane[int64]{
			Reporter:   reporter,
			Sender:     sender,
			Poison:     options.PoisonHandler,
			Unsendable: options.Unsendable,
			Label:      stuckLabel,
			// Identified is nil: every seq identifies its row.
		},
	}

	// Capability implied by configuration must exist unless explicitly waived:
	// a silently missing capability is an outage, not a mode. Without a
	// sequencer (and no WithoutSequencer waiver) rows stay unsequenced and the
	// relay delivers NOTHING while reporting no error — a permanent silent
	// stall. A configured retention window without a Sweeper never
	// sweeps — unbounded table growth surfacing as a disk incident long after
	// the misconfiguration. (The LeaderStore assertion above stays soft:
	// always-leader is a documented single-instance mode, not a configured
	// capability.)
	if !options.SequencerDisabled {
		seq, ok := store.(Sequencer)
		if !ok {
			return nil, errors.New("sequence: store does not implement sequence.Sequencer; " +
				"pass WithoutSequencer() if another relay runs the sequencer for this store")
		}
		r.sequencer = boundedSequencer{inner: seq, ttl: options.OpTimeout}
	}
	if !options.RetentionDisabled {
		ret, ok := store.(Sweeper)
		switch {
		case ok:
			r.retention = boundedRetention{inner: ret, ttl: options.OpTimeout}
		case options.RetentionConfigured:
			// The caller ASKED for a sweep: a silently dead one grows the log
			// unboundedly and surfaces as a disk incident long after the
			// misconfiguration.
			return nil, errors.New("sequence: WithRetention is set but store does not implement sequence.Sweeper")
		default:
			// The default sweep is in play and this store cannot sweep — a
			// legitimate topology (the store prunes itself, or another relay owns
			// the sweep), so not an error. Still say so: an operator who assumed
			// the default was pruning would otherwise learn it from a full disk.
			options.Logger.Info("sequence relay: store has no sequence.Sweeper capability; retention sweep disabled "+
				"(pass WithoutRetention() to make this explicit, and ensure something else prunes the log)",
				"relay", name)
		}
	}
	// Optional, and legitimately absent: without it lag is measured against the
	// relay host's clock instead of the store's (see Clock).
	if c, ok := store.(Clock); ok {
		r.clock = boundedClock{inner: c, ttl: options.OpTimeout}
	}

	return r, nil
}
