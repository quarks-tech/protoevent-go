// Package stream is the MongoDB change-stream outbox relay runtime: a leader
// tails an insert-only outbox collection via a resumable change stream and
// forwards events to a Sender in commit order. It reuses the shared relay
// primitives (Observer, PoisonHandler, LeaderStore) and is dependency-free —
// the resume token crosses the Store boundary as opaque string.
//
// Two audiences use this package. END USERS construct a Relay (NewRelay +
// With* options, typically over mongodb.NewStore) and call Run; RunOnce
// exists for tests and custom drivers (it returns ErrLaneStopped as the
// back-off-and-retry signal). STORE AUTHORS implement the Store / Stream
// contracts (plus relay.LeaderStore) for a new backend — the mongodb module
// is the reference implementation.
package stream

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/quarks-tech/protoevent-go/pkg/eventbus"
	"github.com/quarks-tech/protoevent-go/pkg/transport/outbox"
	"github.com/quarks-tech/protoevent-go/pkg/transport/outbox/internal/leader"
	"github.com/quarks-tech/protoevent-go/pkg/transport/outbox/internal/notify"
	"github.com/quarks-tech/protoevent-go/pkg/transport/outbox/relay"
)

// ErrInvalidated is the sentinel store implementations return from
// Stream.Next when the change stream is invalidated (the outbox collection
// was dropped or renamed). Callers treat it as fatal: the relay stops.
var ErrInvalidated = errors.New("stream: change stream invalidated")

// ErrHistoryLost is the sentinel store implementations return from Stream.Next
// when the resume token has fallen off the oplog (ChangeStreamHistoryLost).
// Fatal in v1 — invoke the break-glass runbook in the outbox README
// ("Runbook: ErrHistoryLost"): a restart cannot fix it and only crash-loops.
var ErrHistoryLost = errors.New("stream: change stream history lost (resume token off oplog)")

// DecodeError reports a change-stream event whose payload failed to decode.
// Store implementations return it from Next with the event's resume position,
// so a relay with a PoisonHandler can park the event and keep the lane moving;
// without one the lane stops (at-least-once, order preserved).
type DecodeError struct {
	ID string // event_id if extractable, else ""
	// ResumeToken is the poison event's token — the relay resumes past it.
	// Extraction is best-effort: "" makes the event non-parkable (the lane
	// stops instead; parking would later persist the empty token and erase
	// the group's position).
	ResumeToken string
	CommitTime  time.Time
	Err         error
}

func (e *DecodeError) Error() string {
	if e.ID != "" {
		return fmt.Sprintf("stream: decode change event %s: %v", e.ID, e.Err)
	}
	return fmt.Sprintf("stream: decode change event: %v", e.Err)
}

func (e *DecodeError) Unwrap() error { return e.Err }

// Store is the change-stream read/offset contract, implemented over MongoDB.
// The resume token is an opaque string (the mongo store casts to/from bson.Raw);
// string gives immutability, comparability, and clean API values. "" == no token.
type Store interface {
	// LoadToken returns the consumer group's stored resume token ("" if none —
	// a new group then starts at "now") and the anchor commit-time (the server-assigned position timestamp: MongoDB clusterTime, a Postgres-style backend's commit timestamp).
	LoadToken(ctx context.Context, name string) (token string, commitTime time.Time, err error)

	// SaveToken persists the resume token + its commitTime for the consumer
	// group. Implementations MUST be monotone in the token's server-assigned
	// position: a save carrying an older position than the stored row must
	// not rewind it (a stale leader finishing a slow window could otherwise
	// overwrite a newer position). commitTime is the coarse anchor for that
	// guard; when the token encoding itself is ordered, implementations
	// SHOULD compare tokens directly so ties within commitTime's granularity
	// cannot rewind either (the mongodb store compares resume-token
	// KeyStrings, which carry the full (T, I) timestamp). persisted reports
	// whether the row now holds THIS token: false means the save was
	// classified stale and skipped — the caller must not advance its local
	// committed-position trackers on a false return, or its lag/cliff
	// reporting diverges from what is actually stored.
	SaveToken(ctx context.Context, name string, token string, commitTime time.Time) (persisted bool, err error)

	// Watch opens a change stream on the outbox collection, filtered to inserts,
	// resumed from token (or from "now" when token is ""). maxAwait is the longest
	// a single Next call may block server-side waiting for events — the relay
	// passes its DrainWindow, making it the single latency knob.
	Watch(ctx context.Context, token string, maxAwait time.Duration) (Stream, error)
}

// Stream is a live change-stream cursor.
type Stream interface {
	// Next returns the next insert event. It blocks up to maxAwait (as passed
	// to Watch) and returns (nil, false, nil) when the window elapses with no
	// event (the caught-up case) — the caller then persists Checkpoint(). On error it
	// returns (nil, false, err):
	//   - ErrInvalidated: the change stream was invalidated (collection
	//     drop/rename) → fatal, the caller stops;
	//   - ErrHistoryLost: the resume token fell off the oplog → fatal;
	//   - *DecodeError: this event's payload failed to decode; carries the
	//     event's resume position so a relay with a PoisonHandler can park it
	//     and resume past it;
	//   - anything else is transient → the caller reopens the stream.
	Next(ctx context.Context) (*Event, bool, error)
	// Checkpoint returns a resumable position at the stream's current head
	// even when no event was delivered (MongoDB: the postBatchResumeToken;
	// a Postgres-style backend: the server's reported WAL end). It serves
	// two purposes: (a) on an empty window, the caller persists it so a
	// caught-up-and-connected consumer's persisted position keeps tracking
	// the head instead of falling behind; and (b) immediately after a fresh
	// Watch (token == ""), before any Next call, the caller persists it as
	// the initial resume baseline — so a first-window send failure under
	// stop-the-lane (which persists nothing itself) still has a prior
	// position to reopen from, instead of restarting at a fresh "now" and
	// silently skipping the failed event.
	//
	// serverTime reports whether commitTime came from the SERVER's clock (decoded
	// out of the token) or is a local-clock substitute the implementation fell back
	// to. Both are fine to persist — the monotone save guard only needs a
	// comparable value — but only a server-derived one may be used to calibrate the
	// relay's clock-offset estimate. Feeding a local substitute into that
	// calibration would mark the estimate "calibrated" while it is really the raw
	// host clock, i.e. exactly the skew-blind measurement the estimate exists to
	// avoid, with no signal that it happened.
	Checkpoint() (token string, commitTime time.Time, serverTime bool)
	Close(ctx context.Context) error
}

// Event is one decoded insert change event.
type Event struct {
	Message     *outbox.Message
	ResumeToken string
	CommitTime  time.Time
}

// options configures a stream relay. It is unexported on purpose: the only
// way to configure a relay is the With* option constructors, whose nil/zero
// guards are then the single validation surface — an exported mutable struct
// alongside ...Option would make invalid states representable and force
// defensive re-defaulting in NewRelay.
type options struct {
	// DrainWindow is the single latency knob: it is both the relay loop tick
	// (the idle/backoff sleep between passes) and the change stream's
	// maxAwaitTime — passed to Store.Watch as maxAwait, bounding how long a
	// single Next call may block server-side waiting for events.
	DrainWindow    time.Duration
	LeaseTTL       time.Duration
	LeaderLockName string // defaults to the relay name
	TokenBatchSize int    // max events processed before a forced token persist

	Logger        *slog.Logger // defaults to a discard logger
	Observer      relay.Observer
	PoisonHandler relay.PoisonHandler
	Unsendable    relay.UnsendableClassifier // nil: every send failure stops the lane
}

// defaultOptions returns the default stream relay configuration. The zero
// relay.Observer discards all signals.
func defaultOptions() options {
	return options{
		DrainWindow:    time.Second,
		LeaseTTL:       15 * time.Second,
		TokenBatchSize: 100,
		Logger:         slog.New(slog.DiscardHandler),
	}
}

// Option configures the relay.
type Option func(*options)

// WithDrainWindow sets the drain window — the single latency knob: both the
// relay loop tick (idle/backoff sleep) and the change stream's maxAwaitTime.
func WithDrainWindow(d time.Duration) Option { return func(o *options) { o.DrainWindow = d } }

// WithLeaseTTL sets the leader-lease TTL.
func WithLeaseTTL(d time.Duration) Option { return func(o *options) { o.LeaseTTL = d } }

// WithLeaderLockName overrides the leader-lock name (defaults to the relay name).
func WithLeaderLockName(s string) Option { return func(o *options) { o.LeaderLockName = s } }

// WithTokenBatchSize sets the max events processed before a forced token persist.
func WithTokenBatchSize(n int) Option { return func(o *options) { o.TokenBatchSize = n } }

// WithLogger sets the error logger. A nil logger is ignored.
func WithLogger(l *slog.Logger) Option {
	return func(o *options) {
		if l != nil {
			o.Logger = l
		}
	}
}

// WithObserver sets the observability sink (a relay.Observer struct of
// nil-able callbacks; the same type both runtimes accept — OnSequenced never
// fires here). The zero value discards all signals.
func WithObserver(obs relay.Observer) Option {
	return func(o *options) { o.Observer = obs }
}

// WithPoisonHandler installs the poison-parking hook: an event whose payload
// fails to decode (*DecodeError with a usable resume token) is handed to h
// and the relay advances past it — retrying a poison event can never succeed,
// so parking it is the only way to keep the lane moving. Send failures are not
// parked by default: a send failure is downstream trouble, and the lane stops
// and redelivers the same event on reopen (order and delivery preserved) —
// unless an UnsendableClassifier claims it, see WithUnsendableClassifier.
// Without a PoisonHandler a poison event stops the lane.
func WithPoisonHandler(h relay.PoisonHandler) Option {
	return func(o *options) { o.PoisonHandler = h }
}

// WithUnsendableClassifier installs the escape hatch for an event the downstream
// transport will never accept: when f reports a send failure permanent for that
// specific event, it is parked through the PoisonHandler and the lane advances
// past it instead of redelivering it on every reopen forever. Every failure f
// does not claim still stops the lane. Requires WithPoisonHandler. See
// relay.UnsendableClassifier for the (narrow) contract f must honor.
func WithUnsendableClassifier(f relay.UnsendableClassifier) Option {
	return func(o *options) { o.Unsendable = f }
}

// Relay tails the outbox change stream for one consumer group and forwards to
// a Sender. A Relay is not safe for concurrent use: Run (or RunOnce) must be
// called from a single goroutine.
type Relay struct {
	name    string
	store   Store
	sender  eventbus.Sender
	options options
	leader  *leader.Elector

	// Runtime state, populated by Run/RunOnce (pkg/.../stream/run.go).
	stream       Stream
	committedCT  time.Time
	lastSavedTok string // last token successfully persisted (skip no-op saves on idle windows)

	// clockOffset is (MongoDB server clock - this host's clock), calibrated from a
	// clusterTime read while the relay was CAUGHT UP, where that reading is the
	// stream head and therefore the server's "now". It lets committedTokenAge
	// measure lag against the present without a query and without folding NTP skew
	// between the pod and the primary into the number. clockCalibrated is false
	// until a first caught-up window happens; see calibrateClock/serverNow.
	clockOffset     time.Duration
	clockCalibrated bool
	// calibratedHead is the head the current offset was derived from, so a stream
	// sitting at an unchanged position cannot recalibrate the estimate backwards.
	calibratedHead time.Time

	// wasLeader tracks the last observed leadership state so transitions —
	// and only transitions — reach OnLeadership/the log (see trackLeadership).
	wasLeader bool

	// stuck escalates a lane that keeps stopping at the same resume token into a
	// single relay.StuckLaneError per episode. Keyed on the raw token so the
	// per-event Progress call allocates nothing. See noteStuck.
	stuck notify.StuckTracker[string]
}

// StuckLaneError is relay.StuckLaneError, re-exported so callers of this package
// need not import relay to match on it. See its doc for the contract; Position
// carries "resume token <tok>" for this runtime.
type StuckLaneError = relay.StuckLaneError

// NewRelay creates a stream relay for the named consumer group. It validates
// the configuration and returns an error when:
//   - name is empty (it keys the offset/token row and is the default leader
//     lock name);
//   - store or sender is nil;
//   - DrainWindow, LeaseTTL, or TokenBatchSize is not strictly positive;
//   - DrainWindow is not strictly less than LeaseTTL/2 (the lease must be
//     renewable within a single drain window).
//
// Note the DrainWindow guard only bounds the *idle* wait inside a window (the
// change stream's maxAwaitTime), not the total time a drainWindow call can
// take: the leader lease is renewed once per RunOnce call, not within a drain
// window, and a single drainWindow can issue up to TokenBatchSize synchronous
// Sender.Send calls before returning. A slow Sender can therefore make one
// drainWindow run longer than LeaseTTL, letting a transient second leader
// acquire the lease and drain an overlapping range while the first is still
// mid-window. At-least-once still holds (the consumer's event_id dedup
// absorbs the overlap), but the single-active-consumer property weakens.
// Operators should size TokenBatchSize x worst-case Sender.Send latency <
// LeaseTTL to keep a window inside one lease term.
func NewRelay(name string, store Store, sender eventbus.Sender, opts ...Option) (*Relay, error) {
	if name == "" {
		return nil, errors.New("stream: name must not be empty (it keys the offset/token row and is the default leader-lock name)")
	}
	if store == nil {
		return nil, errors.New("stream: store must not be nil")
	}
	if sender == nil {
		return nil, errors.New("stream: sender must not be nil")
	}

	options := defaultOptions()
	for _, opt := range opts {
		opt(&options)
	}
	if options.LeaderLockName == "" {
		options.LeaderLockName = name
	}
	if options.DrainWindow <= 0 {
		return nil, fmt.Errorf("stream: DrainWindow must be > 0, got %v", options.DrainWindow)
	}
	if options.LeaseTTL <= 0 {
		return nil, fmt.Errorf("stream: LeaseTTL must be > 0, got %v", options.LeaseTTL)
	}
	if options.TokenBatchSize <= 0 {
		return nil, fmt.Errorf("stream: TokenBatchSize must be > 0, got %d", options.TokenBatchSize)
	}
	if options.DrainWindow >= options.LeaseTTL/2 {
		return nil, fmt.Errorf("stream: DrainWindow (%v) must be < LeaseTTL/2 (%v)", options.DrainWindow, options.LeaseTTL/2)
	}
	if options.Unsendable != nil && options.PoisonHandler == nil {
		return nil, errors.New("stream: WithUnsendableClassifier requires WithPoisonHandler (there is nowhere to park an unsendable event otherwise)")
	}

	// A store implementing only PART of relay.LeaderStore is rejected outright
	// (see relay.CheckLeaderStore): silently degrading a broken lock
	// implementation to always-leader runs dual leaders across replicas.
	ls, err := relay.CheckLeaderStore(store)
	if err != nil {
		return nil, fmt.Errorf("stream: %w", err)
	}
	if ls == nil {
		// Always-leader is a documented single-instance mode, not an error —
		// but it must not be a silent one: a multi-replica deployment over such
		// a store runs dual leaders with no signal until duplicate delivery is
		// noticed.
		options.Logger.Info("stream relay: store has no relay.LeaderStore capability; running always-leader (single-instance mode)",
			"relay", name)
	}

	return &Relay{
		name: name,
		// Every store operation is decorated with its bound at construction —
		// see boundedStore (bounded.go).
		store:   boundedStore{inner: store, ttl: options.LeaseTTL},
		sender:  sender,
		options: options,
		leader:  leader.NewElector(ls, options.LeaderLockName, uuid.NewString(), options.LeaseTTL),
	}, nil
}
