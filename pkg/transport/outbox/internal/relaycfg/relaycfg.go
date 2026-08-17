// Package relaycfg holds the configuration both relay runtimes share: the
// leader-lease settings, the observability sinks, and the failure-policy hooks,
// together with the validation and wiring that turn them into a running relay.
//
// It exists because those were written out twice, and had already drifted: the
// sequence runtime rejected an over-long consumer-group name while the stream
// runtime accepted it, so the same name was legal on one backend and not the
// other. Validation that must agree across runtimes lives here; anything genuinely
// specific to one runtime (page sizes, drain windows, sequencing, retention) stays
// in that runtime's own options.
package relaycfg

import (
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/quarks-tech/protoevent-go/pkg/transport/outbox/internal/leader"
	"github.com/quarks-tech/protoevent-go/pkg/transport/outbox/internal/notify"
	"github.com/quarks-tech/protoevent-go/pkg/transport/outbox/relay"
)

// MaxNameLen bounds the consumer-group and leader-lock names.
//
// The reference SQL schema (tidb/migrations/) keys outbox_offsets, relay_locks,
// and outbox_sequencers on VARCHAR(64): under strict sql_mode a longer name fails
// every tick with a generic 1406 far from the misconfiguration, and under a
// relaxed sql_mode it is SILENTLY TRUNCATED — two groups sharing a 64-char prefix
// would then share one offset row and one leader lock (cross-group event loss plus
// broken mutual exclusion).
//
// Enforced for BOTH runtimes, not only the SQL one, so that a consumer-group name
// is portable: a name accepted here works on every backend, rather than a relay
// starting fine against MongoDB and failing against TiDB.
const MaxNameLen = 64

// Lease is the leadership configuration both runtimes share: how long a lease
// lasts, what it is keyed on, and whether election happens at all. Runtime option
// structs embed it, so their With* constructors stay one-liners over these fields.
//
// The field names stay fully qualified (LeaseTTL, not TTL) because they are read
// through promotion — the useful spelling is options.LeaseTTL at the call sites,
// not relaycfg.Lease.TTL at the declaration.
type Lease struct {
	// LeaseTTL is how long a leader lease is granted for. It bounds failover:
	// ungraceful leader loss stalls the relay for up to one TTL. It is NOT the
	// store-operation bound — see Ops.
	LeaseTTL time.Duration
	// LeaderLockName defaults to the relay name.
	LeaderLockName string
	// LeaderElectionDisabled waives leader election entirely (single-instance
	// mode). Without it, a store that cannot elect is a construction error.
	LeaderElectionDisabled bool
}

// Ops is the store-operation timeout policy both runtimes share. Embedded
// alongside Lease.
//
// It is a SEPARATE knob from LeaseTTL, and used to be the same one. The two
// budgets answer unrelated questions — "how long may a wedged store call hang
// before we give up on it" versus "how long may an ungraceful leader loss stall
// the relay" — and tying them together meant every attempt to shorten failover
// silently shortened every store deadline by the same factor, tightening an I/O
// budget nobody meant to touch. Lowering LeaseTTL is a failover decision; it
// should not decide what counts as a hung query.
type Ops struct {
	// OpTimeout bounds every individual store call (see internal/bound). Neither
	// database/sql nor the mongo driver has a default operation timeout, so
	// without it a call on a wedged connection stalls the single Run goroutine
	// with no error and no log.
	OpTimeout time.Duration
}

// Hooks is the observability and failure-policy configuration both runtimes
// share: where signals go, and what happens to a message the relay cannot
// deliver. Embedded alongside Lease.
type Hooks struct {
	Logger        *slog.Logger
	Observer      relay.Observer
	PoisonHandler relay.PoisonHandler
	Unsendable    relay.UnsendableClassifier
}

// DefaultLease returns the shared lease defaults.
//
// This is the failover stall an ungraceful leader loss costs, and a clean
// shutdown releases the lock explicitly, so only crashes pay it.
//
// 60s, not the 15s this used to be: the lease must be able to CONTAIN a pass, and
// a pass is renewal cadence plus one store call (see Ops.Validate). At 15s against
// the 30s OpTimeout the shipped pair violated that by 2x, so one slow-but-
// successful store call returned onto an expired lease and the relay then drained
// and committed a whole page as a stale leader. Shortening failover below this
// means shortening OpTimeout with it.
func DefaultLease() Lease {
	return Lease{LeaseTTL: 60 * time.Second}
}

// DefaultOps returns the shared store-operation defaults.
func DefaultOps() Ops {
	return Ops{OpTimeout: 30 * time.Second}
}

// DefaultHooks returns the shared hook defaults. The zero relay.Observer discards
// all signals.
//
// The default LOGGER does not: it is slog.Default(), not a discard handler. A
// relay whose lane stops has stopped delivering every message behind the failed
// one, and the defaults compose into exactly that — no Unsendable classifier means
// any send failure stops the lane, and no PoisonHandler means a poison row does
// too. Pairing those with a discarded log and a zero Observer made the
// out-of-the-box configuration wedge in complete silence, which is the one thing an
// at-least-once relay must never do quietly. Pass
// WithLogger(slog.New(slog.DiscardHandler)) to opt back into silence.
func DefaultHooks() Hooks {
	return Hooks{Logger: slog.Default()}
}

// Validate checks the lease configuration for one relay. runtime is the package
// name used in error messages ("sequence"/"stream"), name is the consumer group,
// and tick is the runtime's own pass interval — PollInterval for the sequence
// runtime, DrainWindow for the stream one — named by tickName in errors.
//
// It also applies the LeaderLockName default, so a caller that skips this cannot
// end up with an unnamed lock.
func (l *Lease) Validate(runtime, name, tickName string, tick time.Duration) error {
	if l.LeaderLockName == "" {
		l.LeaderLockName = name
	}
	if len(l.LeaderLockName) > MaxNameLen {
		return fmt.Errorf("%s: LeaderLockName %q exceeds %d bytes (the reference schema's VARCHAR(64) key column)",
			runtime, l.LeaderLockName, MaxNameLen)
	}
	if l.LeaseTTL <= 0 {
		return fmt.Errorf("%s: LeaseTTL must be > 0, got %v", runtime, l.LeaseTTL)
	}
	// The lease is renewed once per pass (and, in the sequence runtime, between
	// pages), so it must be renewable at least twice per TTL or it expires between
	// renewals and leadership silently flaps every tick.
	if tick >= l.LeaseTTL/2 {
		return fmt.Errorf("%s: %s (%v) must be < LeaseTTL/2 (%v)", runtime, tickName, tick, l.LeaseTTL/2)
	}

	return nil
}

// Validate checks the store-operation timeout for one relay. tick is the
// runtime's own pass interval, named by tickName in errors — the same pair
// Lease.Validate takes.
//
// The tick guard matters most for the stream runtime, where DrainWindow is also
// the change stream's maxAwaitTime: a Next call legitimately blocks server-side
// for a whole window, so an OpTimeout at or below it would fire on every idle
// window and reopen a cursor that was working perfectly.
// leaseTTL relates the two budgets to each other, which neither guard above nor
// Lease.Validate can do alone: each of them checks the tick against ONE budget,
// while a stale-leader pass is a violation of their SUM.
func (o *Ops) Validate(runtime, tickName string, tick, leaseTTL time.Duration) error {
	if o.OpTimeout <= 0 {
		return fmt.Errorf("%s: OpTimeout must be > 0, got %v", runtime, o.OpTimeout)
	}
	if tick >= o.OpTimeout {
		return fmt.Errorf("%s: %s (%v) must be < OpTimeout (%v) — a single store call has to be "+
			"able to outlast one pass, or every pass times out mid-flight", runtime, tickName, tick, o.OpTimeout)
	}
	// The lease must be able to CONTAIN a pass. The lease is renewed between
	// pages, so in a caught-up steady state exactly once per tick; a store call
	// that runs longer than what is left of the lease returns onto an EXPIRED
	// lease, and the relay then commits an offset as a stale leader. Nothing
	// fences that write — CommitOffset is an unconditional GREATEST upsert with no
	// holder check — so two leaders interleave positions and TOTAL ORDER breaks.
	// event_id dedup absorbs the duplicates; it cannot restore order.
	//
	// This bounds ONE store call. A drain window whose every call is slow can
	// still overrun the lease in aggregate (worst for the stream runtime, which
	// may make TokenBatchSize bounded calls inside one window); closing that
	// completely needs a holder-checked commit, not a bigger number here.
	if tick+o.OpTimeout >= leaseTTL {
		return fmt.Errorf("%s: %s (%v) + OpTimeout (%v) must be < LeaseTTL (%v) — the lease is renewed "+
			"once per pass, so a longer store call returns onto an expired lease and the relay commits "+
			"as a stale leader; raise LeaseTTL or lower OpTimeout",
			runtime, tickName, tick, o.OpTimeout, leaseTTL)
	}

	return nil
}

// Validate checks that the configured hooks compose into something that can
// actually dispose of a message.
func (h *Hooks) Validate(runtime string) error {
	if h.Unsendable != nil && h.PoisonHandler == nil {
		return fmt.Errorf("%s: WithUnsendableClassifier requires WithPoisonHandler "+
			"(there is nowhere to park an unsendable message otherwise)", runtime)
	}

	return nil
}

// ValidateName checks the consumer-group name itself, which keys the offset/token
// row and is the default leader-lock name.
func ValidateName(runtime, name string) error {
	if name == "" {
		return fmt.Errorf("%s: name must not be empty "+
			"(it keys the offset row and is the default leader-lock name)", runtime)
	}
	if len(name) > MaxNameLen {
		return fmt.Errorf("%s: name %q exceeds %d bytes (the reference schema's VARCHAR(64) key columns; "+
			"a relaxed sql_mode would silently truncate it into another group's offset row and leader lock)",
			runtime, name, MaxNameLen)
	}

	return nil
}

// Elector resolves the store's leadership capability and builds the elector.
//
// A store that cannot elect is an ERROR unless the caller waived election with
// WithoutLeaderElection. Leadership is the one capability whose silent absence is
// not a degraded mode but duplicate delivery: every replica would consider itself
// leader and forward the whole log. Discovery is a plain type assertion — a store
// that meant to elect but whose method set drifted fails it, and the error says
// what to implement.
func (l *Lease) Elector(runtime string, store any) (*leader.Elector, error) {
	ls, ok := store.(relay.LeaderStore)
	switch {
	case l.LeaderElectionDisabled:
		ls = nil // explicit single-instance mode, whatever the store can do
	case !ok:
		return nil, fmt.Errorf("%s: store does not implement relay.LeaderStore "+
			"(TryAcquireLeaderLock and ReleaseLeaderLock); pass WithoutLeaderElection() if this is a "+
			"single-instance deployment — running several replicas over a store without election "+
			"makes every replica a leader and delivers the whole log from each of them", runtime)
	}

	return leader.NewElector(ls, l.LeaderLockName, uuid.NewString(), l.LeaseTTL), nil
}

// Reporter builds the signal sink for one relay.
func (h *Hooks) Reporter(runtime, name string) *notify.Reporter {
	return &notify.Reporter{
		Runtime:  runtime,
		Name:     name,
		Observer: h.Observer,
		Logger:   h.Logger,
	}
}

// ErrNilSender and ErrNilStore keep the two runtimes' nil checks identical.
var (
	errNilStore  = errors.New("store must not be nil")
	errNilSender = errors.New("sender must not be nil")
)

// ValidateDeps checks the two non-optional dependencies every relay needs.
// store is taken as any because each runtime has its own store interface.
func ValidateDeps(runtime string, store, sender any) error {
	if store == nil {
		return fmt.Errorf("%s: %w", runtime, errNilStore)
	}
	if sender == nil {
		return fmt.Errorf("%s: %w", runtime, errNilSender)
	}

	return nil
}
