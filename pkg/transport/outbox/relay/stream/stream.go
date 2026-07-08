// Package stream is the MongoDB change-stream outbox relay runtime: a leader
// tails an insert-only outbox collection via a resumable change stream and
// forwards events to a Sender in commit order. It reuses the shared relay
// primitives (Observer, Logger, LeaderStore) and is dependency-free — the
// resume token crosses the StreamStore boundary as opaque string.
package stream

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/quarks-tech/protoevent-go/pkg/eventbus"
	"github.com/quarks-tech/protoevent-go/pkg/transport/outbox"
	"github.com/quarks-tech/protoevent-go/pkg/transport/outbox/relay"
)

// StreamStore is the change-stream read/offset contract, implemented over MongoDB.
// The resume token is an opaque string (the mongo store casts to/from bson.Raw);
// string gives immutability, comparability, and clean API values. "" == no token.
type StreamStore interface {
	// LoadToken returns the consumer group's stored resume token ("" if none —
	// a new group then starts at "now") and the anchor clusterTime.
	LoadToken(ctx context.Context, name string) (token string, clusterTime time.Time, err error)

	// SaveToken persists the resume token + its clusterTime for the consumer group.
	SaveToken(ctx context.Context, name string, token string, clusterTime time.Time) error

	// Watch opens a change stream on the outbox collection, filtered to inserts,
	// resumed from token (or from "now" when token is "").
	Watch(ctx context.Context, token string) (Stream, error)
}

// Stream is a live change-stream cursor.
type Stream interface {
	// Next returns the next insert event. It blocks up to the drain window and
	// returns (nil, false, nil) when the window elapses with no event (the
	// caught-up case) — the caller then persists PBRT(). Returns (nil,false,err)
	// on a stream error (transient → caller reopens; fatal → caller stops).
	Next(ctx context.Context) (*Event, bool, error)
	// PBRT returns the postBatchResumeToken after an empty window, so a caught-up
	// consumer's persisted position keeps tracking the oplog head.
	PBRT() (token string, clusterTime time.Time)
	Close(ctx context.Context) error
}

// Event is one decoded insert change event.
type Event struct {
	Message     *outbox.Message
	ResumeToken string
	ClusterTime time.Time
	Invalidate  bool // true on an invalidate change event → fatal (design §6d)
}

// Options configures a stream relay.
type Options struct {
	DrainWindow    time.Duration // change stream maxAwaitTime; also the loop tick
	LeaseTTL       time.Duration
	LeaderLockName string // defaults to the relay name
	TokenBatchSize int    // max events processed before a forced token persist

	Logger       relay.Logger
	Observer     relay.Observer
	ErrorHandler func(ctx context.Context, msg *outbox.Message, err error)
}

// DefaultOptions returns the default stream relay configuration.
func DefaultOptions() Options {
	return Options{
		DrainWindow:    time.Second,
		LeaseTTL:       15 * time.Second,
		TokenBatchSize: 100,
		Observer:       nopObserver{},
		Logger:         nopLogger{},
	}
}

type nopObserver struct{}

func (nopObserver) ObserveDrained(string, int, time.Duration, bool) {}
func (nopObserver) ObserveError(string, error)                      {}

// nopLogger is the default relay.Logger: it discards everything.
type nopLogger struct{}

func (nopLogger) Errorf(string, ...any) {}

// Option configures Options.
type Option func(*Options)

func WithDrainWindow(d time.Duration) Option { return func(o *Options) { o.DrainWindow = d } }
func WithLeaseTTL(d time.Duration) Option    { return func(o *Options) { o.LeaseTTL = d } }
func WithLeaderLockName(s string) Option     { return func(o *Options) { o.LeaderLockName = s } }
func WithTokenBatchSize(n int) Option        { return func(o *Options) { o.TokenBatchSize = n } }

// WithLogger sets the error logger. A nil logger is ignored.
func WithLogger(l relay.Logger) Option {
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
func WithErrorHandler(h func(ctx context.Context, msg *outbox.Message, err error)) Option {
	return func(o *Options) { o.ErrorHandler = h }
}

// Relay tails the outbox change stream for one consumer group and forwards to a Sender.
type Relay struct {
	name    string
	store   StreamStore
	sender  eventbus.Sender
	options Options
	leader  *relay.LeaderElector

	// Runtime state, populated by Run/RunOnce (pkg/.../stream/run.go).
	stream      Stream
	committedCT time.Time
}

// NewRelay creates a stream relay for the named consumer group. It returns an
// error if DrainWindow is not strictly less than LeaseTTL/2 (the lease must be
// renewable within a single drain window).
//
// Note this guard only bounds the *idle* wait inside a window (the change
// stream's maxAwaitTime), not the total time a drainWindow call can take: the
// leader lease is renewed once per RunOnce call, not within a drain window,
// and a single drainWindow can issue up to TokenBatchSize synchronous
// Sender.Send calls before returning. A slow Sender can therefore make one
// drainWindow run longer than LeaseTTL, letting a transient second leader
// acquire the lease and drain an overlapping range while the first is still
// mid-window. At-least-once still holds (the consumer's event_id dedup
// absorbs the overlap), but the single-active-consumer property weakens.
// Operators should size TokenBatchSize x worst-case Sender.Send latency <
// LeaseTTL to keep a window inside one lease term (see design doc §8.2).
func NewRelay(name string, store StreamStore, sender eventbus.Sender, opts ...Option) (*Relay, error) {
	options := DefaultOptions()
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

	return &Relay{
		name:    name,
		store:   store,
		sender:  sender,
		options: options,
		leader:  relay.NewLeaderElector(store, options.LeaderLockName, uuid.NewString(), options.LeaseTTL),
	}, nil
}
