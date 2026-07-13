// Package stream is the MongoDB change-stream outbox relay runtime: a leader
// tails an insert-only outbox collection via a resumable change stream and
// forwards events to a Sender in commit order. It reuses the shared relay
// primitives (Observer, ErrorHandler, LeaderStore) and is dependency-free —
// the resume token crosses the Store boundary as opaque string.
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
	"github.com/quarks-tech/protoevent-go/pkg/transport/outbox/relay"
)

// ErrStreamInvalidated is the sentinel store implementations return from
// Stream.Next when the change stream is invalidated (the outbox collection
// was dropped or renamed). Callers treat it as fatal: the relay stops.
var ErrStreamInvalidated = errors.New("stream: change stream invalidated")

// ErrHistoryLost is the sentinel store implementations return from Stream.Next
// when the resume token has fallen off the oplog (ChangeStreamHistoryLost).
// Fatal in v1 — invoke the break-glass runbook.
var ErrHistoryLost = errors.New("stream: change stream history lost (resume token off oplog)")

// DecodeError reports a change-stream event whose payload failed to decode.
// Store implementations return it from Next with the event's resume position,
// so a relay with an ErrorHandler can park the event and keep the lane moving;
// without one the lane stops (at-least-once, order preserved).
type DecodeError struct {
	ID          string // event_id if extractable, else ""
	ResumeToken string // token of the poison event — the relay resumes past it
	ClusterTime time.Time
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
	// a new group then starts at "now") and the anchor clusterTime.
	LoadToken(ctx context.Context, name string) (token string, clusterTime time.Time, err error)

	// SaveToken persists the resume token + its clusterTime for the consumer
	// group. Implementations SHOULD be monotone in clusterTime: a save carrying
	// an older clusterTime than the stored row must not rewind it (a stale
	// leader finishing a slow window could otherwise overwrite a newer position).
	SaveToken(ctx context.Context, name string, token string, clusterTime time.Time) error

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
	// event (the caught-up case) — the caller then persists PBRT(). On error it
	// returns (nil, false, err):
	//   - ErrStreamInvalidated: the change stream was invalidated (collection
	//     drop/rename) → fatal, the caller stops;
	//   - ErrHistoryLost: the resume token fell off the oplog → fatal;
	//   - *DecodeError: this event's payload failed to decode; carries the
	//     event's resume position so a relay with an ErrorHandler can park it
	//     and resume past it;
	//   - anything else is transient → the caller reopens the stream.
	Next(ctx context.Context) (*Event, bool, error)
	// PBRT returns the postBatchResumeToken. It serves two purposes: (a) on an
	// empty window, the caller persists it so a caught-up-and-connected
	// consumer's persisted position keeps tracking the oplog head instead of
	// falling behind; and (b) immediately after a fresh Watch (token == ""),
	// before any Next call, the caller persists it as the initial resume
	// baseline — so a first-window send failure under stop-the-lane (which
	// persists nothing itself) still has a prior position to reopen from,
	// instead of restarting at a fresh "now" and silently skipping the failed
	// event.
	PBRT() (token string, clusterTime time.Time)
	Close(ctx context.Context) error
}

// Event is one decoded insert change event.
type Event struct {
	Message     *outbox.Message
	ResumeToken string
	ClusterTime time.Time
}

// Options configures a stream relay.
type Options struct {
	// DrainWindow is the single latency knob: it is both the relay loop tick
	// (the idle/backoff sleep between passes) and the change stream's
	// maxAwaitTime — passed to Store.Watch as maxAwait, bounding how long a
	// single Next call may block server-side waiting for events.
	DrainWindow    time.Duration
	LeaseTTL       time.Duration
	LeaderLockName string // defaults to the relay name
	TokenBatchSize int    // max events processed before a forced token persist

	Logger       *slog.Logger
	Observer     relay.Observer
	ErrorHandler relay.ErrorHandler
}

// DefaultOptions returns the default stream relay configuration.
func DefaultOptions() Options {
	return Options{
		DrainWindow:    time.Second,
		LeaseTTL:       15 * time.Second,
		TokenBatchSize: 100,
		Observer:       relay.NopObserver(),
		Logger:         slog.New(slog.DiscardHandler),
	}
}

// Option configures Options.
type Option func(*Options)

func WithDrainWindow(d time.Duration) Option { return func(o *Options) { o.DrainWindow = d } }
func WithLeaseTTL(d time.Duration) Option    { return func(o *Options) { o.LeaseTTL = d } }
func WithLeaderLockName(s string) Option     { return func(o *Options) { o.LeaderLockName = s } }
func WithTokenBatchSize(n int) Option        { return func(o *Options) { o.TokenBatchSize = n } }

// WithLogger sets the error logger. A nil logger is ignored.
func WithLogger(l *slog.Logger) Option {
	return func(o *Options) {
		if l != nil {
			o.Logger = l
		}
	}
}

// WithObserver sets the observability sink. A nil observer is ignored.
func WithObserver(obs relay.Observer) Option {
	return func(o *Options) {
		if obs != nil {
			o.Observer = obs
		}
	}
}

// WithErrorHandler switches send-failure handling from stop-the-lane (default,
// order-preserving) to park-and-continue.
func WithErrorHandler(h relay.ErrorHandler) Option {
	return func(o *Options) { o.ErrorHandler = h }
}

// Relay tails the outbox change stream for one consumer group and forwards to
// a Sender. A Relay is not safe for concurrent use: Run (or RunOnce) must be
// called from a single goroutine.
type Relay struct {
	name    string
	store   Store
	sender  eventbus.Sender
	options Options
	leader  *relay.LeaderElector

	// Runtime state, populated by Run/RunOnce (pkg/.../stream/run.go).
	stream       Stream
	committedCT  time.Time
	lastSavedTok string // last token successfully persisted (skip no-op saves on idle windows)
}

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
// LeaseTTL to keep a window inside one lease term (see design doc §8.2).
func NewRelay(name string, store Store, sender eventbus.Sender, opts ...Option) (*Relay, error) {
	if name == "" {
		return nil, fmt.Errorf("stream: name must not be empty")
	}
	if store == nil {
		return nil, fmt.Errorf("stream: store must not be nil")
	}
	if sender == nil {
		return nil, fmt.Errorf("stream: sender must not be nil")
	}

	options := DefaultOptions()
	for _, opt := range opts {
		opt(&options)
	}
	// Re-default after applying options: a raw Option can nil these past the
	// With* nil guards.
	if options.Observer == nil {
		options.Observer = relay.NopObserver()
	}
	if options.Logger == nil {
		options.Logger = slog.New(slog.DiscardHandler)
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

	ls, _ := store.(relay.LeaderStore)

	return &Relay{
		name:    name,
		store:   store,
		sender:  sender,
		options: options,
		leader:  relay.NewLeaderElector(ls, options.LeaderLockName, uuid.NewString(), options.LeaseTTL),
	}, nil
}
