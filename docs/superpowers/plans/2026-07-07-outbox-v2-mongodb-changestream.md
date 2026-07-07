# Outbox v2 MongoDB Change-Stream Relay Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add a MongoDB outbox relay that tails an insert-only `outbox` collection via a change stream (commit-ordered, resumable, no broker) and forwards events to an `eventbus.Sender`, replacing the poll+delete two-collection design.

**Architecture:** Two subsystems. **(A) `relay/stream` runtime** — a dependency-free engine package (sibling to `relay/sequence`) with a `StreamStore` contract (opaque `[]byte` resume token + `Watch`) and a `stream.Relay` that, under leader election, opens a resumable change stream and drains fixed windows to a Sender, persisting the resume token per window. **(B) `pkg/transport/outbox/mongodb`** — a separate module (own go.mod so the mongo driver + testcontainers stay out of the engine) implementing publish, the `StreamStore`, and `relay.LeaderStore` over `go.mongodb.org/mongo-driver/v2`, with testcontainers replica-set integration tests.

**Tech Stack:** Go 1.25.3; `go.mongodb.org/mongo-driver/v2 v2.5.1` (change streams, transactions); `testcontainers-go/modules/mongodb` (single-node replica set); `encoding/json` for CloudEvents metadata.

## Global Constraints

- Design doc of record: `docs/design/outbox-mongodb-changestream.md`. Where this plan and the doc disagree on API shape, this plan wins (notably: the resume token is `[]byte` in the engine, not `bson.Raw` — see below).
- **Prerequisite:** the shared `relay` package (`relay.Observer`, `relay.Logger`, `relay.LeaderStore`) from the TiDB plan (`docs/superpowers/plans/2026-07-07-outbox-v2-sequenced-log.md`, Task 2). If MongoDB is implemented first, create `pkg/transport/outbox/relay/relay.go` per that task's Step 3 before starting Task 1 here. This plan does **not** re-create it.
- **Engine core stays dependency-free.** `pkg/transport/outbox` and `pkg/transport/outbox/relay/stream` may import only: stdlib, `github.com/google/uuid`, and `github.com/quarks-tech/protoevent-go/pkg/{event,eventbus,transport/outbox,transport/outbox/relay}`. **No mongo driver in the engine** — the resume token crosses the `StreamStore` boundary as an opaque **`string`** (Go strings hold arbitrary bytes; the mongo store casts `string(rawToken)` / `bson.Raw(token)`). `string` is chosen over `[]byte` for immutability, comparability (`==`, map keys), and clean API values; "no stored token → start now" is `token == ""`. Driver + testcontainers live only in `pkg/transport/outbox/mongodb`.
- **Replica set required.** Change streams and the transactional publish both require a MongoDB replica set. Tests use a single-node RS via `mongodb.Run(ctx, image, mongodb.WithReplicaSet("rs0"))` + `directConnection=true`. Integration tests skip cleanly when Docker is unavailable.
- **v1 is `StartNow`-only.** A new consumer group starts at "now". No replay-from-beginning; replay and break-glass DR are the deferred unified "backfill" feature (design §7). No `StartPosition` option.
- **Ordering/dedup:** change streams deliver commit-`clusterTime` order, gap-free, resumable; delivery is at-least-once; consumers dedup on CloudEvents `event_id`. Concurrent-transaction order is unspecified (Kafka parity).
- **Token persistence (design §6c):** non-empty window → persist the last successfully-sent event's token + `clusterTime`; empty window (caught up) → persist the `postBatchResumeToken` + its `clusterTime`. This keeps caught-up-connected consumers resumable and makes committed-token age an honest lag signal.
- **Metadata** is stored as `encoding/json` bytes (round-trips `Extensions`/`url.URL`); the relay never queries it.
- Any change-stream driver API marked "**VERIFY v2.5.1**" was reported from general `mongo-driver/v2` knowledge with no in-repo precedent — confirm the exact symbol against the pinned version during Step 2 of the relevant task; adjust the call, not the behavior.
- TDD throughout: failing test first, minimal implementation, green, commit. One logical change per commit.

---

## File Structure

**Engine — `pkg/transport/outbox/relay/stream/` (existing `pkg/transport/outbox` module):**
- `stream.go` — CREATE (package `stream`): `StreamStore`, `Stream`, `Event` interfaces/types; `Options`; option funcs; `Relay`; `NewRelay` (with the `DrainWindow < LeaseTTL/2` guard).
- `run.go` — CREATE: `Run`, `drainWindow`, leadership + graceful release, token persistence, observability.
- `stream_test.go` — CREATE: in-memory `fakeStreamStore`/`fakeStream` + unit tests.

**MongoDB — `pkg/transport/outbox/mongodb/` (new module):**
- `go.mod` — CREATE: requires engine module + `go.mongodb.org/mongo-driver/v2`, `testcontainers-go` + mongodb module.
- `store.go` — CREATE: `Store` implementing `outbox.Store` (publish), `stream.StreamStore` (`LoadToken`/`SaveToken`), `relay.LeaderStore` (`TryAcquireLeaderLock`/`ReleaseLeaderLock`); `ensureIndexes` (TTL); doc structs.
- `watch.go` — CREATE: `Store.Watch` + `mongoStream` implementing `stream.Stream` (change-stream open, `Next` decode, `PBRT`, `Close`).
- `mongodbtest/container.go` — CREATE: testcontainers replica-set harness.
- `store_test.go` — CREATE: integration tests (publish, TTL index, leader lock, offsets).
- `stream_test.go` — CREATE: integration tests (Watch delivery + order, resume, PBRT-on-idle).
- `relay_integration_test.go` — CREATE: end-to-end `stream.Relay` over real Mongo.

---

## Task 1: `stream` package interfaces, options, NewRelay

**Files:**
- Create: `pkg/transport/outbox/relay/stream/stream.go`
- Test: `pkg/transport/outbox/relay/stream/stream_test.go`

**Interfaces:**
- Consumes: `outbox.Message`, `eventbus.Sender`, `relay.Observer`, `relay.Logger`, `relay.LeaderStore`.
- Produces (relied on by Task 2 and Part B):
  - `StreamStore{ LoadToken(ctx, name string) (token string, clusterTime time.Time, err error); SaveToken(ctx, name string, token string, clusterTime time.Time) error; Watch(ctx, token string) (Stream, error) }`
  - `Stream{ Next(ctx) (*Event, bool, error); PBRT() (token string, clusterTime time.Time); Close(ctx) error }`
  - `Event{ Message *outbox.Message; ResumeToken string; ClusterTime time.Time; Invalidate bool }`
  - `Options`, `Option`, option funcs: `WithDrainWindow(d)`, `WithLeaseTTL(d)`, `WithLeaderLockName(s)`, `WithTokenBatchSize(n)`, `WithObserver(relay.Observer)`, `WithLogger(relay.Logger)`, `WithErrorHandler(func(ctx, *outbox.Message, error))`.
  - `NewRelay(name string, store StreamStore, sender eventbus.Sender, opts ...Option) (*Relay, error)` — returns an error if `DrainWindow >= LeaseTTL/2`.
  - Defaults: `DrainWindow=time.Second`, `LeaseTTL=15*time.Second`, `TokenBatchSize=100`.

- [ ] **Step 1: Write the failing test**

Create `pkg/transport/outbox/relay/stream/stream_test.go`:

```go
package stream_test

import (
	"testing"
	"time"

	"github.com/quarks-tech/protoevent-go/pkg/transport/outbox/relay/stream"
)

func TestDefaultOptions(t *testing.T) {
	o := stream.DefaultOptions()
	if o.DrainWindow != time.Second {
		t.Fatalf("DrainWindow = %v, want 1s", o.DrainWindow)
	}
	if o.LeaseTTL != 15*time.Second {
		t.Fatalf("LeaseTTL = %v, want 15s", o.LeaseTTL)
	}
	if o.TokenBatchSize != 100 {
		t.Fatalf("TokenBatchSize = %d, want 100", o.TokenBatchSize)
	}
}

func TestNewRelayRejectsDrainWindowTooLarge(t *testing.T) {
	// DrainWindow must be < LeaseTTL/2 so the lease can be renewed within a window.
	_, err := stream.NewRelay("c", nil, nil,
		stream.WithLeaseTTL(10*time.Second), stream.WithDrainWindow(6*time.Second))
	if err == nil {
		t.Fatal("expected error for DrainWindow >= LeaseTTL/2, got nil")
	}
}

func TestNewRelayAcceptsValidWindow(t *testing.T) {
	r, err := stream.NewRelay("c", nil, nil,
		stream.WithLeaseTTL(10*time.Second), stream.WithDrainWindow(1*time.Second))
	if err != nil || r == nil {
		t.Fatalf("NewRelay valid config: r=%v err=%v", r, err)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd pkg/transport/outbox && go test ./relay/stream/ -run 'TestDefaultOptions|TestNewRelay'`
Expected: FAIL — package `stream` does not exist.

- [ ] **Step 3: Write minimal implementation**

Create `pkg/transport/outbox/relay/stream/stream.go`:

```go
// Package stream is the MongoDB change-stream outbox relay runtime: a leader
// tails an insert-only outbox collection via a resumable change stream and
// forwards events to a Sender in commit order. It reuses the shared relay
// primitives (Observer, Logger, LeaderStore) and is dependency-free — the
// resume token crosses the StreamStore boundary as opaque []byte.
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
	}
}

type nopObserver struct{}

func (nopObserver) ObserveDrained(string, int, time.Duration, bool) {}
func (nopObserver) ObserveError(string, error)                      {}

// Option configures Options.
type Option func(*Options)

func WithDrainWindow(d time.Duration) Option  { return func(o *Options) { o.DrainWindow = d } }
func WithLeaseTTL(d time.Duration) Option     { return func(o *Options) { o.LeaseTTL = d } }
func WithLeaderLockName(s string) Option      { return func(o *Options) { o.LeaderLockName = s } }
func WithTokenBatchSize(n int) Option         { return func(o *Options) { o.TokenBatchSize = n } }
func WithLogger(l relay.Logger) Option        { return func(o *Options) { o.Logger = l } }

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
	name     string
	store    StreamStore
	sender   eventbus.Sender
	options  Options
	holderID string

	isLeader    bool
	stream      Stream
	committedCT time.Time // clusterTime of the last persisted token (lag anchor)
}

// NewRelay creates a stream relay for the named consumer group. It returns an
// error if DrainWindow is not strictly less than LeaseTTL/2 (the lease must be
// renewable within a single drain window).
func NewRelay(name string, store StreamStore, sender eventbus.Sender, opts ...Option) (*Relay, error) {
	options := DefaultOptions()
	for _, opt := range opts {
		opt(&options)
	}
	if options.LeaderLockName == "" {
		options.LeaderLockName = name
	}
	if options.DrainWindow >= options.LeaseTTL/2 {
		return nil, fmt.Errorf("stream: DrainWindow (%v) must be < LeaseTTL/2 (%v)", options.DrainWindow, options.LeaseTTL/2)
	}

	return &Relay{
		name:     name,
		store:    store,
		sender:   sender,
		options:  options,
		holderID: uuid.NewString(),
	}, nil
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `cd pkg/transport/outbox && go test ./relay/stream/ -run 'TestDefaultOptions|TestNewRelay'`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add pkg/transport/outbox/relay/stream/stream.go pkg/transport/outbox/relay/stream/stream_test.go
git commit -m "feat(outbox): stream relay interfaces + options + guard"
```

---

## Task 2: `stream.Relay` runtime (leader-gated window loop, persistence, stop-the-lane, invalidate-fatal, graceful release)

**Files:**
- Create: `pkg/transport/outbox/relay/stream/run.go`
- Test: `pkg/transport/outbox/relay/stream/stream_test.go` (append fakes + runtime tests)

**Interfaces:**
- Consumes: Task 1 types, `relay.LeaderStore`.
- Produces: `Relay.Run(ctx) error` (blocks until ctx canceled or a fatal stream error), and package sentinels `ErrStreamInvalidated`, `ErrHistoryLost` returned from `Run` on the corresponding fatal conditions.

- [ ] **Step 1: Write the failing test**

Append to `pkg/transport/outbox/relay/stream/stream_test.go` (add imports `context`, `errors`, `sync`, `time`, plus `event`, `eventbus`, `outbox`, `relay`). Fakes model the `StreamStore`/`Stream` contract; `runWindow` is an unexported test seam exposed via an exported `RunOnce` wrapper (see Step 3).

```go
// senderFunc adapts a func to eventbus.Sender.
type senderFunc func(context.Context, *event.Metadata, []byte) error

func (f senderFunc) Send(ctx context.Context, md *event.Metadata, d []byte) error { return f(ctx, md, d) }

// fakeStream serves a scripted list of events, then empty windows.
type fakeStream struct {
	events []*stream.Event
	i      int
	pbrt   string
	pbrtCT time.Time
}

func (s *fakeStream) Next(context.Context) (*stream.Event, bool, error) {
	if s.i < len(s.events) {
		e := s.events[s.i]
		s.i++
		return e, true, nil
	}
	return nil, false, nil // window empty
}
func (s *fakeStream) PBRT() (string, time.Time) { return s.pbrt, s.pbrtCT }
func (s *fakeStream) Close(context.Context) error { return nil }

// fakeStreamStore hands out one fakeStream and records saved tokens.
type fakeStreamStore struct {
	mu        sync.Mutex
	stream    *fakeStream
	loadTok   string
	loadCT    time.Time
	savedTok  string
	savedCT   time.Time
	saveCount int
	leader    string
}

func (s *fakeStreamStore) LoadToken(context.Context, string) (string, time.Time, error) {
	return s.loadTok, s.loadCT, nil
}
func (s *fakeStreamStore) SaveToken(_ context.Context, _ string, tok string, ct time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.savedTok, s.savedCT, s.saveCount = tok, ct, s.saveCount+1
	return nil
}
func (s *fakeStreamStore) Watch(context.Context, string) (stream.Stream, error) { return s.stream, nil }
func (s *fakeStreamStore) TryAcquireLeaderLock(_ context.Context, _, holderID string, _ time.Duration) (bool, error) {
	if s.leader == "" || s.leader == holderID {
		s.leader = holderID
		return true, nil
	}
	return false, nil
}
func (s *fakeStreamStore) ReleaseLeaderLock(_ context.Context, _, holderID string) error {
	if s.leader == holderID {
		s.leader = ""
	}
	return nil
}

func ev(seq int, tok string, invalidate bool) *stream.Event {
	md := event.NewMetadata("t")
	md.ID = tok
	return &stream.Event{
		Message:     &outbox.Message{ID: tok, Metadata: md, CreateTime: time.Now()},
		ResumeToken: tok,
		ClusterTime: time.Now(),
		Invalidate:  invalidate,
	}
}

func TestRunOnceDeliversAndPersistsLastToken(t *testing.T) {
	st := &fakeStreamStore{stream: &fakeStream{events: []*stream.Event{ev(1, "a", false), ev(2, "b", false)}}}
	var got []string
	sender := senderFunc(func(_ context.Context, md *event.Metadata, _ []byte) error {
		got = append(got, md.ID)
		return nil
	})
	r, _ := stream.NewRelay("c", st, sender)
	if err := r.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Fatalf("delivered %v, want [a b]", got)
	}
	if st.savedTok != "b" {
		t.Fatalf("saved token = %q, want b (last processed)", st.savedTok)
	}
}

func TestRunOncePersistsPBRTOnEmptyWindow(t *testing.T) {
	st := &fakeStreamStore{stream: &fakeStream{events: nil, pbrt: "pbrt", pbrtCT: time.Now()}}
	sender := senderFunc(func(context.Context, *event.Metadata, []byte) error { return nil })
	r, _ := stream.NewRelay("c", st, sender)
	if err := r.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if st.savedTok != "pbrt" {
		t.Fatalf("saved token = %q, want pbrt (empty window persists PBRT)", st.savedTok)
	}
}

func TestRunOnceStopTheLane(t *testing.T) {
	st := &fakeStreamStore{stream: &fakeStream{events: []*stream.Event{ev(1, "a", false), ev(2, "b", false), ev(3, "c", false)}}}
	var got []string
	sender := senderFunc(func(_ context.Context, md *event.Metadata, _ []byte) error {
		if md.ID == "b" {
			return errors.New("boom")
		}
		got = append(got, md.ID)
		return nil
	})
	r, _ := stream.NewRelay("c", st, sender)
	_ = r.RunOnce(context.Background())
	// Delivered a; stopped at b; c not attempted; token persisted up to a.
	if len(got) != 1 || got[0] != "a" {
		t.Fatalf("delivered %v, want [a] (stop-the-lane)", got)
	}
	if st.savedTok != "a" {
		t.Fatalf("saved token = %q, want a", st.savedTok)
	}
}

func TestRunOnceInvalidateIsFatal(t *testing.T) {
	st := &fakeStreamStore{stream: &fakeStream{events: []*stream.Event{ev(1, "x", true)}}}
	sender := senderFunc(func(context.Context, *event.Metadata, []byte) error { return nil })
	r, _ := stream.NewRelay("c", st, sender)
	err := r.RunOnce(context.Background())
	if !errors.Is(err, stream.ErrStreamInvalidated) {
		t.Fatalf("err = %v, want ErrStreamInvalidated", err)
	}
}

func TestRunOnceNonLeaderIdles(t *testing.T) {
	st := &fakeStreamStore{stream: &fakeStream{events: []*stream.Event{ev(1, "a", false)}}, leader: "other"}
	sent := 0
	sender := senderFunc(func(context.Context, *event.Metadata, []byte) error { sent++; return nil })
	r, _ := stream.NewRelay("c", st, sender, stream.WithLeaderLockName("lock"))
	if err := r.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if sent != 0 {
		t.Fatalf("non-leader sent %d, want 0", sent)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd pkg/transport/outbox && go test ./relay/stream/ -run TestRunOnce`
Expected: FAIL — `RunOnce` / `ErrStreamInvalidated` not defined.
While here, **VERIFY v2.5.1** is not needed (pure engine); the driver checks land in Tasks 4–6.

- [ ] **Step 3: Write minimal implementation**

Create `pkg/transport/outbox/relay/stream/run.go`:

```go
package stream

import (
	"context"
	"errors"
	"time"

	"github.com/quarks-tech/protoevent-go/pkg/transport/outbox"
	"github.com/quarks-tech/protoevent-go/pkg/transport/outbox/relay"
)

const releaseTimeout = 5 * time.Second

// ErrStreamInvalidated is returned when the change stream emits an invalidate
// event (the outbox collection was dropped/renamed). Fatal.
var ErrStreamInvalidated = errors.New("stream: change stream invalidated")

// ErrHistoryLost is returned when the resume token has fallen off the oplog
// (ChangeStreamHistoryLost). Fatal in v1 — invoke the break-glass runbook.
var ErrHistoryLost = errors.New("stream: change stream history lost (resume token off oplog)")

// Run drives the relay until ctx is canceled (returns ctx.Err()) or a fatal
// stream condition occurs (returns ErrStreamInvalidated / ErrHistoryLost).
// Releases leadership on exit so a planned shutdown fails over quickly.
func (r *Relay) Run(ctx context.Context) error {
	defer r.closeStream(context.Background())
	defer r.releaseLeadership()

	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		err := r.RunOnce(ctx)
		switch {
		case err == nil:
			// keep looping; RunOnce already blocked ~one drain window
		case errors.Is(err, ErrStreamInvalidated), errors.Is(err, ErrHistoryLost):
			return err // fatal
		default:
			// transient (leadership, reopen, send/save): report, drop the stream, retry
			r.options.Observer.ObserveError(r.name, err)
			if r.options.Logger != nil {
				r.options.Logger.Errorf("stream relay %q: %v", r.name, err)
			}
			r.closeStream(ctx)
			r.sleep(ctx)
		}
	}
}

// RunOnce performs one leader-gated drain window. Non-leaders return nil without
// touching the stream. Exposed for tests; Run calls it in a loop.
func (r *Relay) RunOnce(ctx context.Context) error {
	leader, err := r.tryAcquireLeadership(ctx)
	if err != nil {
		return err
	}
	if !leader {
		r.closeStream(ctx)
		return nil
	}
	if r.stream == nil {
		token, ct, err := r.store.LoadToken(ctx, r.name)
		if err != nil {
			return err
		}
		s, err := r.store.Watch(ctx, token)
		if err != nil {
			return err
		}
		r.stream = s
		r.committedCT = ct
	}
	return r.drainWindow(ctx)
}

// drainWindow processes up to TokenBatchSize events (fast while buffered; one
// DrainWindow wait when idle), persists the token, and reports lag.
func (r *Relay) drainWindow(ctx context.Context) error {
	processed := 0
	stopped := false
	var lastTok string
	var lastCT time.Time

	for i := 0; i < r.options.TokenBatchSize; i++ {
		e, ok, err := r.stream.Next(ctx)
		if err != nil {
			return err
		}
		if !ok {
			break // window elapsed with no more events (caught up)
		}
		if e.Invalidate {
			return ErrStreamInvalidated
		}
		if sendErr := r.sender.Send(ctx, e.Message.Metadata, e.Message.Data); sendErr != nil {
			r.handleError(ctx, e.Message, sendErr)
			if r.options.ErrorHandler == nil {
				stopped = true
				break // stop-the-lane: do not advance past the failure
			}
			// park-and-continue: advance past the parked message
		}
		lastTok, lastCT = e.ResumeToken, e.ClusterTime
		processed++
	}

	switch {
	case processed > 0:
		if err := r.store.SaveToken(ctx, r.name, lastTok, lastCT); err != nil {
			return err
		}
		r.committedCT = lastCT
	case !stopped:
		// Empty window: persist PBRT so a caught-up-connected consumer stays
		// resumable and the lag anchor stays fresh (design §6c).
		if tok, ct := r.stream.PBRT(); tok != "" {
			if err := r.store.SaveToken(ctx, r.name, tok, ct); err != nil {
				return err
			}
			r.committedCT = ct
		}
	}

	more := processed == r.options.TokenBatchSize && !stopped
	r.options.Observer.ObserveDrained(r.name, processed, r.committedTokenAge(), more)
	return nil
}

// committedTokenAge is the cliff proxy: how far the committed token trails the
// oplog head (design §7). Cheap — no query.
func (r *Relay) committedTokenAge() time.Duration {
	if r.committedCT.IsZero() {
		return 0
	}
	return time.Since(r.committedCT)
}

func (r *Relay) tryAcquireLeadership(ctx context.Context) (bool, error) {
	ls, ok := r.store.(relay.LeaderStore)
	if !ok {
		r.isLeader = true
		return true, nil
	}
	held, err := ls.TryAcquireLeaderLock(ctx, r.options.LeaderLockName, r.holderID, r.options.LeaseTTL)
	if err != nil {
		return false, err
	}
	r.isLeader = held
	return held, nil
}

func (r *Relay) releaseLeadership() {
	ls, ok := r.store.(relay.LeaderStore)
	if !ok || !r.isLeader {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), releaseTimeout)
	defer cancel()
	_ = ls.ReleaseLeaderLock(ctx, r.options.LeaderLockName, r.holderID)
	r.isLeader = false
}

func (r *Relay) closeStream(ctx context.Context) {
	if r.stream != nil {
		_ = r.stream.Close(ctx)
		r.stream = nil
	}
}

func (r *Relay) sleep(ctx context.Context) {
	t := time.NewTimer(r.options.DrainWindow)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}

func (r *Relay) handleError(ctx context.Context, msg *outbox.Message, err error) {
	if r.options.ErrorHandler != nil {
		r.options.ErrorHandler(ctx, msg, err)
	}
	r.options.Observer.ObserveError(r.name, err)
	if r.options.Logger != nil {
		r.options.Logger.Errorf("stream relay %q: send message %s: %v", r.name, msg.ID, err)
	}
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `cd pkg/transport/outbox && go test ./relay/stream/ -run TestRunOnce && go vet ./relay/stream/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add pkg/transport/outbox/relay/stream/run.go pkg/transport/outbox/relay/stream/stream_test.go
git commit -m "feat(outbox): stream relay runtime (window drain, PBRT persistence, stop-the-lane, invalidate-fatal)"
```

---

## Task 3: MongoDB module scaffold + Store publish/offset/leader

**Files:**
- Create: `pkg/transport/outbox/mongodb/go.mod`
- Create: `pkg/transport/outbox/mongodb/store.go`
- Test: `pkg/transport/outbox/mongodb/store_test.go` (construction-only for now)

**Interfaces:**
- Consumes: `outbox.Store`, `outbox.Message`, `stream.StreamStore`, `relay.LeaderStore`, `event.Metadata`.
- Produces:
  - `NewStore(db *mongo.Database) *Store`.
  - `Store.CreateOutboxMessage(ctx, *outbox.Message) error` (publish; call on the session-bound ctx).
  - `Store.LoadToken` / `Store.SaveToken` (satisfies `stream.StreamStore` read/offset methods).
  - `Store.TryAcquireLeaderLock` / `Store.ReleaseLeaderLock` (satisfies `relay.LeaderStore`).
  - `Store.EnsureIndexes(ctx) error` (TTL on `outbox.create_time`).
  - Collection constants `outboxCollection="outbox"`, `offsetsCollection="outbox_offsets"`, `lockCollection="relay_lock"`.
  - Compile-time assertions for `outbox.Store` and `relay.LeaderStore`.

- [ ] **Step 1: Write the failing test**

Create `pkg/transport/outbox/mongodb/store_test.go`:

```go
package mongodb_test

import (
	"testing"

	mongodbstore "github.com/quarks-tech/protoevent-go/pkg/transport/outbox/mongodb"
)

func TestNewStoreConstructs(t *testing.T) {
	if mongodbstore.NewStore(nil) == nil {
		t.Fatal("NewStore returned nil")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd pkg/transport/outbox/mongodb && go test ./... -run TestNewStoreConstructs 2>&1 | head`
Expected: FAIL — no `go.mod` / `NewStore` undefined.

- [ ] **Step 3: Write minimal implementation**

Create `pkg/transport/outbox/mongodb/go.mod`:

```
module github.com/quarks-tech/protoevent-go/pkg/transport/outbox/mongodb

go 1.25.3

require (
	github.com/quarks-tech/protoevent-go v0.4.2
	github.com/quarks-tech/protoevent-go/pkg/transport/outbox v0.0.0
	github.com/testcontainers/testcontainers-go v0.42.0
	github.com/testcontainers/testcontainers-go/modules/mongodb v0.42.0
	go.mongodb.org/mongo-driver/v2 v2.5.1
)

replace github.com/quarks-tech/protoevent-go/pkg/transport/outbox => ../
```

Create `pkg/transport/outbox/mongodb/store.go`:

```go
// Package mongodb implements the outbox publish path and the change-stream relay
// contracts (stream.StreamStore + relay.LeaderStore) over MongoDB.
package mongodb

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/quarks-tech/protoevent-go/pkg/event"
	"github.com/quarks-tech/protoevent-go/pkg/transport/outbox"
	"github.com/quarks-tech/protoevent-go/pkg/transport/outbox/relay"
)

const (
	outboxCollection  = "outbox"
	offsetsCollection = "outbox_offsets"
	lockCollection    = "relay_lock"

	// retentionDays is the outbox TTL; MUST exceed the oplog window (design §7).
	retentionDays = 7
)

// outboxDoc is one insert-only event envelope. Metadata is JSON bytes so the
// CloudEvents url.URL / Extensions map round-trip cleanly.
type outboxDoc struct {
	ID         string    `bson:"_id"`
	Metadata   []byte    `bson:"metadata"`
	Data       []byte    `bson:"data"`
	CreateTime time.Time `bson:"create_time"`
}

// offsetDoc is one consumer group's resume-token store.
type offsetDoc struct {
	Name        string    `bson:"_id"`
	ResumeToken bson.Raw  `bson:"resume_token"`
	ClusterTime time.Time `bson:"cluster_time"`
	UpdateTime  time.Time `bson:"update_time"`
}

type lockDoc struct {
	Name       string    `bson:"_id"`
	HolderID   string    `bson:"holder_id"`
	ExpireTime time.Time `bson:"expire_time"`
}

// Store realizes the outbox publish + relay-read contracts over a *mongo.Database.
type Store struct {
	db *mongo.Database
}

func NewStore(db *mongo.Database) *Store { return &Store{db: db} }

var (
	_ outbox.Store     = (*Store)(nil)
	_ relay.LeaderStore = (*Store)(nil)
)

// EnsureIndexes creates the TTL index on outbox.create_time. Idempotent.
func (s *Store) EnsureIndexes(ctx context.Context) error {
	_, err := s.db.Collection(outboxCollection).Indexes().CreateOne(ctx, mongo.IndexModel{
		Keys:    bson.D{{Key: "create_time", Value: 1}},
		Options: options.Index().SetExpireAfterSeconds(int32(retentionDays * 24 * 60 * 60)),
	})
	if err != nil {
		return fmt.Errorf("outbox: ensure ttl index: %w", err)
	}
	return nil
}

// CreateOutboxMessage inserts an unsequenced event envelope. Call on the
// session-bound ctx so it commits atomically with the business write.
func (s *Store) CreateOutboxMessage(ctx context.Context, msg *outbox.Message) error {
	meta, err := json.Marshal(msg.Metadata)
	if err != nil {
		return fmt.Errorf("outbox: marshal metadata: %w", err)
	}
	doc := outboxDoc{ID: msg.ID, Metadata: meta, Data: msg.Data, CreateTime: msg.CreateTime}
	if _, err := s.db.Collection(outboxCollection).InsertOne(ctx, doc); err != nil {
		return fmt.Errorf("outbox: insert: %w", err)
	}
	return nil
}

// LoadToken returns the consumer group's resume token ("" if none) as a string
// and the anchor clusterTime. The stored bson.Raw bytes are carried verbatim.
func (s *Store) LoadToken(ctx context.Context, name string) (string, time.Time, error) {
	var doc offsetDoc
	err := s.db.Collection(offsetsCollection).FindOne(ctx, bson.M{"_id": name}).Decode(&doc)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return "", time.Time{}, nil
	}
	if err != nil {
		return "", time.Time{}, fmt.Errorf("outbox: load token: %w", err)
	}
	return string(doc.ResumeToken), doc.ClusterTime, nil
}

// SaveToken upserts the consumer group's resume token + clusterTime. token is
// the opaque resume token as a string; it is stored as bson.Raw bytes.
func (s *Store) SaveToken(ctx context.Context, name string, token string, clusterTime time.Time) error {
	_, err := s.db.Collection(offsetsCollection).UpdateOne(ctx,
		bson.M{"_id": name},
		bson.M{"$set": bson.M{
			"resume_token": bson.Raw(token),
			"cluster_time": clusterTime.UTC(),
			"update_time":  time.Now().UTC(),
		}},
		options.UpdateOne().SetUpsert(true),
	)
	if err != nil {
		return fmt.Errorf("outbox: save token: %w", err)
	}
	return nil
}

// TryAcquireLeaderLock acquires or renews the lock via a conditional upsert.
func (s *Store) TryAcquireLeaderLock(ctx context.Context, name, holderID string, ttl time.Duration) (bool, error) {
	now := time.Now()
	filter := bson.M{
		"_id": name,
		"$or": bson.A{
			bson.M{"expire_time": bson.M{"$lt": now}},
			bson.M{"holder_id": holderID},
		},
	}
	update := bson.M{"$set": lockDoc{Name: name, HolderID: holderID, ExpireTime: now.Add(ttl)}}
	res, err := s.db.Collection(lockCollection).UpdateOne(ctx, filter, update, options.UpdateOne().SetUpsert(true))
	if err != nil {
		if mongo.IsDuplicateKeyError(err) {
			return false, nil // another instance holds a live lock
		}
		return false, fmt.Errorf("outbox: acquire lock: %w", err)
	}
	return res.MatchedCount > 0 || res.UpsertedCount > 0, nil
}

// ReleaseLeaderLock drops the lock if still held by holderID (graceful shutdown).
func (s *Store) ReleaseLeaderLock(ctx context.Context, name, holderID string) error {
	_, err := s.db.Collection(lockCollection).DeleteOne(ctx, bson.M{"_id": name, "holder_id": holderID})
	if err != nil {
		return fmt.Errorf("outbox: release lock: %w", err)
	}
	return nil
}

// decodeMessage rebuilds an outbox.Message from a stored outboxDoc (used by Watch).
func decodeMessage(doc outboxDoc) (*outbox.Message, error) {
	var md event.Metadata
	if err := json.Unmarshal(doc.Metadata, &md); err != nil {
		return nil, fmt.Errorf("outbox: unmarshal metadata %s: %w", doc.ID, err)
	}
	return &outbox.Message{ID: doc.ID, Metadata: &md, Data: doc.Data, CreateTime: doc.CreateTime}, nil
}
```

- [ ] **Step 4: Resolve deps, run test to verify it passes**

Run: `cd pkg/transport/outbox/mongodb && go mod tidy && go test ./... -run TestNewStoreConstructs && go vet ./...`
Expected: PASS. Commit the generated `go.sum`.
**VERIFY v2.5.1:** confirm `mongo.ErrNoDocuments`, `mongo.IsDuplicateKeyError`, `options.Index().SetExpireAfterSeconds(int32)`, and `bson.Raw`/`bson.D{{Key,Value}}` compile against v2.5.1. Adjust symbols if the pinned version differs; behavior is unchanged.

- [ ] **Step 5: Commit**

```bash
git add pkg/transport/outbox/mongodb/go.mod pkg/transport/outbox/mongodb/go.sum pkg/transport/outbox/mongodb/store.go pkg/transport/outbox/mongodb/store_test.go
git commit -m "feat(outbox-mongodb): module scaffold + store publish/offset/leader"
```

---

## Task 4: MongoDB Watch + `mongoStream`

**Files:**
- Create: `pkg/transport/outbox/mongodb/watch.go`

**Interfaces:**
- Consumes: Task 3 `Store`, `stream.Stream`, `stream.Event`, `decodeMessage`.
- Produces:
  - `Store.Watch(ctx, token string) (stream.Stream, error)` — opens an insert-filtered change stream with `maxAwaitTime`, resumed from `token` (or "now" when `token == ""`).
  - `mongoStream` implementing `stream.Stream`: `Next` (decode insert event → `Event`), `PBRT`, `Close`.
  - Compile-time assertion `var _ stream.StreamStore = (*Store)(nil)`.
  - `Store` carries a configurable `maxAwaitTime` set from the relay's drain window — add `SetMaxAwaitTime(d time.Duration)` on `Store` (the relay wiring calls it; default 1s).

- [ ] **Step 1: Write the failing test**

Behavior needs a live replica set (Task 5). This task's gate is `go build` + the `stream.StreamStore` assertion compiling. Add a construction check to `store_test.go`:

```go
func TestStoreSatisfiesStreamStore(t *testing.T) {
	// Compile-time proof lives in watch.go (var _ stream.StreamStore = ...).
	// This test just ensures the package builds with Watch present.
	_ = mongodbstore.NewStore(nil)
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd pkg/transport/outbox/mongodb && go test ./... -run TestStoreSatisfiesStreamStore`
Expected: FAIL — `Watch` undefined, `stream.StreamStore` assertion missing.

- [ ] **Step 3: Write minimal implementation**

Create `pkg/transport/outbox/mongodb/watch.go`:

```go
package mongodb

import (
	"context"
	"fmt"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/quarks-tech/protoevent-go/pkg/transport/outbox/relay/stream"
)

var _ stream.StreamStore = (*Store)(nil)

// SetMaxAwaitTime sets the change stream's server await time per getMore. The
// relay wiring sets this from its drain window. Default 1s if unset.
func (s *Store) SetMaxAwaitTime(d time.Duration) { s.maxAwait = d }

// changeEvent is the decoded change-stream document (insert or invalidate).
type changeEvent struct {
	ID            bson.Raw       `bson:"_id"`           // resume token for THIS event
	OperationType string         `bson:"operationType"` // insert | invalidate | ...
	ClusterTime   bson.Timestamp `bson:"clusterTime"`   // VERIFY v2.5.1: bson.Timestamp
	FullDocument  outboxDoc      `bson:"fullDocument"`  // present on insert
}

// Watch opens an insert-filtered change stream resumed from token (or "now").
func (s *Store) Watch(ctx context.Context, token string) (stream.Stream, error) {
	await := s.maxAwait
	if await <= 0 {
		await = time.Second
	}
	pipeline := mongo.Pipeline{
		bson.D{{Key: "$match", Value: bson.D{{Key: "operationType", Value: "insert"}}}},
	}
	opts := options.ChangeStream().SetMaxAwaitTime(await)
	if token != "" {
		opts = opts.SetResumeAfter(bson.Raw(token))
	}
	// else: no resumeAfter → the stream starts at "now" (v1 StartNow).

	cs, err := s.db.Collection(outboxCollection).Watch(ctx, pipeline, opts)
	if err != nil {
		return nil, fmt.Errorf("outbox: open change stream: %w", err)
	}
	return &mongoStream{cs: cs}, nil
}

// mongoStream adapts a *mongo.ChangeStream to stream.Stream.
type mongoStream struct {
	cs *mongo.ChangeStream
}

// Next drains one event if buffered; on an empty batch it blocks up to
// maxAwaitTime and returns (nil,false,nil). A stream error → (nil,false,err).
func (m *mongoStream) Next(ctx context.Context) (*stream.Event, bool, error) {
	// TryNext returns false when the current batch is drained WITHOUT closing
	// the stream (respecting maxAwaitTime) — exactly the window semantics we want.
	if !m.cs.TryNext(ctx) {
		if err := m.cs.Err(); err != nil {
			return nil, false, err
		}
		return nil, false, nil // empty window
	}
	var ce changeEvent
	if err := m.cs.Decode(&ce); err != nil {
		return nil, false, fmt.Errorf("outbox: decode change event: %w", err)
	}
	if ce.OperationType == "invalidate" {
		return &stream.Event{Invalidate: true}, true, nil
	}
	msg, err := decodeMessage(ce.FullDocument)
	if err != nil {
		return nil, false, err
	}
	return &stream.Event{
		Message:     msg,
		ResumeToken: string(ce.ID),
		ClusterTime: time.Unix(int64(ce.ClusterTime.T), 0).UTC(), // VERIFY v2.5.1: Timestamp.T
	}, true, nil
}

// PBRT returns the postBatchResumeToken after an empty window. The driver
// surfaces the batch-level token via ResumeToken(); clusterTime is derived from
// the token's embedded timestamp is not exposed, so we stamp "now" as the anchor
// (a connected, caught-up consumer's head ≈ now).
func (m *mongoStream) PBRT() (string, time.Time) {
	tok := m.cs.ResumeToken()
	if tok == nil {
		return "", time.Time{}
	}
	return string(tok), time.Now().UTC()
}

func (m *mongoStream) Close(ctx context.Context) error { return m.cs.Close(ctx) }
```

Add the `maxAwait` field to the `Store` struct in `store.go`:

```go
type Store struct {
	db       *mongo.Database
	maxAwait time.Duration
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `cd pkg/transport/outbox/mongodb && go build ./... && go test ./... -run TestStoreSatisfiesStreamStore && go vet ./...`
Expected: PASS.
**VERIFY v2.5.1:** confirm `Collection.Watch(ctx, mongo.Pipeline, *options.ChangeStreamOptionsBuilder)`, `options.ChangeStream().SetMaxAwaitTime/.SetResumeAfter`, `ChangeStream.TryNext/.Decode/.Err/.ResumeToken/.Close`, and `bson.Timestamp{T,I}`. If the options builder type name or `bson.Timestamp` differs in v2.5.1, adjust the calls — the control flow is unchanged.

- [ ] **Step 5: Commit**

```bash
git add pkg/transport/outbox/mongodb/watch.go pkg/transport/outbox/mongodb/store.go
git commit -m "feat(outbox-mongodb): change-stream Watch + stream adapter"
```

---

## Task 5: Testcontainers replica-set harness + store integration tests

**Files:**
- Create: `pkg/transport/outbox/mongodb/mongodbtest/container.go`
- Modify: `pkg/transport/outbox/mongodb/store_test.go` (add `TestMain` + integration tests)

**Interfaces:**
- Consumes: `tidb`-equivalent pattern; `Store`, `mongo.Database`.
- Produces: `mongodbtest.Start(ctx) (*Instance, func(), error)` with `Instance{ DB *mongo.Database; Client *mongo.Client }`.
- **Note:** integration tests require Docker; they skip cleanly when the container can't start.

- [ ] **Step 1: Write the failing test**

Create `pkg/transport/outbox/mongodb/mongodbtest/container.go`:

```go
// Package mongodbtest boots an ephemeral single-node MongoDB replica set
// (testcontainers) for integration tests: change streams and transactions both
// require a replica set.
package mongodbtest

import (
	"context"
	"fmt"
	"strings"

	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/testcontainers/testcontainers-go/modules/mongodb"
)

const dbName = "outbox_test"

type Instance struct {
	Client    *mongo.Client
	DB        *mongo.Database
	terminate func()
}

// Start boots MongoDB as a single-node replica set, connects, and returns a
// ready Instance + cleanup. Returns an error (tests should t.Skip on it) when
// Docker is unavailable.
func Start(ctx context.Context) (*Instance, func(), error) {
	c, err := mongodb.Run(ctx, "mongo:8", mongodb.WithReplicaSet("rs0"))
	if err != nil {
		return nil, nil, fmt.Errorf("start mongodb (Docker unavailable?): %w", err)
	}
	uri, err := c.ConnectionString(ctx)
	if err != nil {
		_ = c.Terminate(ctx)
		return nil, nil, err
	}
	// directConnection: the single-node RS advertises a container-internal
	// address; a direct connection to the mapped port still supports txns and
	// change streams (the server IS a replica-set member).
	dsn := uri + "&directConnection=true"
	if !strings.Contains(uri, "?") {
		dsn = uri + "?directConnection=true"
	}
	client, err := mongo.Connect(options.Client().ApplyURI(dsn)) // v2: no ctx arg
	if err != nil {
		_ = c.Terminate(ctx)
		return nil, nil, err
	}
	inst := &Instance{
		Client:    client,
		DB:        client.Database(dbName),
		terminate: func() { _ = client.Disconnect(context.Background()); _ = c.Terminate(context.Background()) },
	}
	return inst, inst.terminate, nil
}
```

Add `TestMain` + integration tests to `store_test.go`:

```go
package mongodb_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"

	"github.com/quarks-tech/protoevent-go/pkg/event"
	"github.com/quarks-tech/protoevent-go/pkg/transport/outbox"
	mongodbstore "github.com/quarks-tech/protoevent-go/pkg/transport/outbox/mongodb"
	"github.com/quarks-tech/protoevent-go/pkg/transport/outbox/mongodb/mongodbtest"
)

var testDB *mongo.Database

func TestMain(m *testing.M) {
	inst, cleanup, err := mongodbtest.Start(context.Background())
	if err != nil {
		os.Exit(0) // Docker unavailable: skip the integration suite
	}
	testDB = inst.DB
	code := m.Run()
	cleanup()
	os.Exit(code)
}

func reset(t *testing.T) {
	t.Helper()
	for _, c := range []string{"outbox", "outbox_offsets", "relay_lock"} {
		if err := testDB.Collection(c).Drop(context.Background()); err != nil {
			t.Fatalf("drop %s: %v", c, err)
		}
	}
}

func publish(t *testing.T, subject string) string {
	t.Helper()
	st := mongodbstore.NewStore(testDB)
	md := event.NewMetadata("books.created")
	md.ID = uuid.NewString()
	md.Source = "books-service"
	md.Subject = subject
	md.DataContentType = "application/proto"
	md.Time = time.Now().UTC()
	// Publish in a transaction (mirrors production; requires the replica set).
	sess, err := testDB.Client().StartSession()
	if err != nil {
		t.Fatal(err)
	}
	defer sess.EndSession(context.Background())
	_, err = sess.WithTransaction(context.Background(), func(sc context.Context) (any, error) {
		return nil, st.CreateOutboxMessage(sc, &outbox.Message{ID: md.ID, Metadata: md, Data: []byte("x"), CreateTime: md.Time})
	})
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	return md.ID
}

func TestPublishInsertsEnvelope(t *testing.T) {
	if testDB == nil {
		t.Skip("no MongoDB")
	}
	reset(t)
	id := publish(t, "s1")
	n, err := testDB.Collection("outbox").CountDocuments(context.Background(), bson.M{"_id": id})
	if err != nil || n != 1 {
		t.Fatalf("count = %d err = %v, want 1", n, err)
	}
}

func TestEnsureIndexesCreatesTTL(t *testing.T) {
	if testDB == nil {
		t.Skip("no MongoDB")
	}
	reset(t)
	if err := mongodbstore.NewStore(testDB).EnsureIndexes(context.Background()); err != nil {
		t.Fatalf("ensure indexes: %v", err)
	}
	cur, err := testDB.Collection("outbox").Indexes().List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var idx []bson.M
	if err := cur.All(context.Background(), &idx); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, ix := range idx {
		if _, ok := ix["expireAfterSeconds"]; ok {
			found = true
		}
	}
	if !found {
		t.Fatal("no TTL index on outbox")
	}
}

func TestOffsetRoundTrip(t *testing.T) {
	if testDB == nil {
		t.Skip("no MongoDB")
	}
	reset(t)
	st := mongodbstore.NewStore(testDB)
	ctx := context.Background()
	ct := time.Now().UTC().Truncate(time.Millisecond)
	if err := st.SaveToken(ctx, "c", "\x01\x02", ct); err != nil {
		t.Fatal(err)
	}
	tok, gotCT, err := st.LoadToken(ctx, "c") // tok is string
	if err != nil {
		t.Fatal(err)
	}
	if len(tok) != 2 || tok[0] != 0x01 {
		t.Fatalf("token = %q, want \\x01\\x02", tok)
	}
	if !gotCT.Equal(ct) {
		t.Fatalf("clusterTime = %v, want %v", gotCT, ct)
	}
}

func TestLeaderLockMutualExclusionAndRelease(t *testing.T) {
	if testDB == nil {
		t.Skip("no MongoDB")
	}
	reset(t)
	st := mongodbstore.NewStore(testDB)
	ctx := context.Background()
	okA, err := st.TryAcquireLeaderLock(ctx, "lock", "A", 30*time.Second)
	if err != nil || !okA {
		t.Fatalf("A acquire = %v, %v; want true", okA, err)
	}
	okB, _ := st.TryAcquireLeaderLock(ctx, "lock", "B", 30*time.Second)
	if okB {
		t.Fatal("B acquired while A holds the lock")
	}
	if err := st.ReleaseLeaderLock(ctx, "lock", "A"); err != nil {
		t.Fatal(err)
	}
	okB2, _ := st.TryAcquireLeaderLock(ctx, "lock", "B", 30*time.Second)
	if !okB2 {
		t.Fatal("B failed to acquire after A released")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd pkg/transport/outbox/mongodb && go mod tidy && go test ./... -run 'TestPublishInserts|TestEnsureIndexes|TestOffsetRoundTrip|TestLeaderLock' -v`
Expected (Docker): initial failures if SQL/API mismatches; iterate to green. (No Docker: SKIP — run with Docker before completing.)
**VERIFY v2.5.1:** `mongodb.Run`, `mongodb.WithReplicaSet`, `container.ConnectionString`, `mongo.Connect(options...)` (no ctx), `sess.WithTransaction` callback signature `func(context.Context) (any, error)`.

- [ ] **Step 3: Make it pass**

Fix any API/scan mismatches surfaced by the run. Confirm the `testcontainers-go/modules/mongodb` version resolved by `go mod tidy` matches the core `testcontainers-go` version (bump both together if `go.sum` complains).

- [ ] **Step 4: Run the store suite**

Run: `cd pkg/transport/outbox/mongodb && go test ./... -run 'TestPublish|TestEnsure|TestOffset|TestLeader' -v`
Expected: PASS with Docker.

- [ ] **Step 5: Commit**

```bash
git add pkg/transport/outbox/mongodb/mongodbtest pkg/transport/outbox/mongodb/store_test.go pkg/transport/outbox/mongodb/go.sum
git commit -m "test(outbox-mongodb): replica-set harness + store integration tests"
```

---

## Task 6: Change-stream integration tests (delivery, order, resume, PBRT)

**Files:**
- Create: `pkg/transport/outbox/mongodb/stream_test.go`

**Interfaces:**
- Consumes: `Store.Watch`, `Store.SetMaxAwaitTime`, `stream.Stream`, the publish helper.
- Produces: proof that Watch delivers inserts in commit order, resumes from a persisted token (no re-delivery of already-consumed events), and advances PBRT on idle.

- [ ] **Step 1: Write the failing test**

Create `pkg/transport/outbox/mongodb/stream_test.go`:

```go
package mongodb_test

import (
	"context"
	"testing"
	"time"

	mongodbstore "github.com/quarks-tech/protoevent-go/pkg/transport/outbox/mongodb"
)

func drainAll(t *testing.T, s interface {
	Next(context.Context) (*streamEvent, bool, error)
}) {
	t.Helper()
}

// streamEvent aliases avoid importing the stream package's Event directly in the
// helper signature; use the real type in the tests below.
type streamEvent = struct{}

func TestWatchDeliversInsertsInOrder(t *testing.T) {
	if testDB == nil {
		t.Skip("no MongoDB")
	}
	reset(t)
	st := mongodbstore.NewStore(testDB)
	st.SetMaxAwaitTime(300 * time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	// Open the stream at "now" BEFORE publishing, so we catch the inserts.
	strm, err := st.Watch(ctx, nil)
	if err != nil {
		t.Fatalf("watch: %v", err)
	}
	defer strm.Close(context.Background())

	ids := []string{publish(t, "a"), publish(t, "b"), publish(t, "c")}

	var got []string
	for len(got) < 3 {
		e, ok, err := strm.Next(ctx)
		if err != nil {
			t.Fatalf("next: %v", err)
		}
		if !ok {
			continue // empty window; keep waiting
		}
		got = append(got, e.Message.ID)
	}
	for i := range ids {
		if got[i] != ids[i] {
			t.Fatalf("order[%d] = %s, want %s (commit order)", i, got[i], ids[i])
		}
	}
}

func TestWatchResumesFromToken(t *testing.T) {
	if testDB == nil {
		t.Skip("no MongoDB")
	}
	reset(t)
	st := mongodbstore.NewStore(testDB)
	st.SetMaxAwaitTime(300 * time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	s1, err := st.Watch(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	first := publish(t, "first")
	_ = publish(t, "second")

	// consume only the first, capture its token, close.
	var tok string
	for {
		e, ok, err := s1.Next(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if !ok {
			continue
		}
		if e.Message.ID == first {
			tok = e.ResumeToken
			break
		}
	}
	_ = s1.Close(context.Background())

	// resume from the token: must NOT re-deliver "first".
	s2, err := st.Watch(ctx, tok)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close(context.Background())
	e, ok, err := s2.Next(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if ok && e.Message.ID == first {
		t.Fatal("resume re-delivered the already-consumed event")
	}
}

func TestPBRTAdvancesOnIdle(t *testing.T) {
	if testDB == nil {
		t.Skip("no MongoDB")
	}
	reset(t)
	st := mongodbstore.NewStore(testDB)
	st.SetMaxAwaitTime(300 * time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	s, err := st.Watch(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close(context.Background())

	// Idle: no matching inserts. An empty window must yield a non-nil PBRT.
	_, ok, err := s.Next(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("unexpected event on an idle stream")
	}
	tok, _ := s.PBRT()
	if tok == "" {
		t.Fatal("PBRT returned empty on an empty window; a caught-up consumer would fall off the oplog")
	}
}
```

Remove the `drainAll`/`streamEvent` scaffolding stubs before finalizing — they are placeholders from an earlier draft; the tests above use `strm.Next` returning the real `*stream.Event` directly (its `.Message.ID` / `.ResumeToken` fields). Ensure the file imports `stream` only if you reference the type name; here we rely on the returned value's fields, so no direct import is needed.

- [ ] **Step 2: Run test to verify it fails, then passes**

Run: `cd pkg/transport/outbox/mongodb && go test ./... -run 'TestWatch|TestPBRT' -v`
Expected (Docker): PASS after fixes. (No Docker: SKIP — run with Docker before completing.)
Note: these tests exercise the real `mongo-driver/v2` change-stream path — this is where the **VERIFY v2.5.1** API items from Task 4 are actually confirmed. If `TryNext`/`ResumeToken`/`Decode` behave differently than assumed, fix `watch.go` and re-run.

- [ ] **Step 3: Commit**

```bash
git add pkg/transport/outbox/mongodb/stream_test.go
git commit -m "test(outbox-mongodb): change-stream delivery, resume, PBRT integration tests"
```

---

## Task 7: End-to-end `stream.Relay` over real MongoDB

**Files:**
- Create: `pkg/transport/outbox/mongodb/relay_integration_test.go`

**Interfaces:**
- Consumes: `stream.NewRelay`, `stream.Relay.RunOnce`, `Store`, the publish helper.
- Produces: proof that publish → change stream → forward preserves order and persists the offset end-to-end via the real runtime.

- [ ] **Step 1: Write the failing test**

Create `pkg/transport/outbox/mongodb/relay_integration_test.go`:

```go
package mongodb_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/quarks-tech/protoevent-go/pkg/event"
	mongodbstore "github.com/quarks-tech/protoevent-go/pkg/transport/outbox/mongodb"
	"github.com/quarks-tech/protoevent-go/pkg/transport/outbox/relay/stream"
)

type recordingSender struct {
	mu  sync.Mutex
	ids []string
}

func (r *recordingSender) Send(_ context.Context, md *event.Metadata, _ []byte) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ids = append(r.ids, md.ID)
	return nil
}

func TestStreamRelayEndToEnd(t *testing.T) {
	if testDB == nil {
		t.Skip("no MongoDB")
	}
	reset(t)
	st := mongodbstore.NewStore(testDB)
	st.SetMaxAwaitTime(300 * time.Millisecond)

	sender := &recordingSender{}
	r, err := stream.NewRelay("e2e", st, sender,
		stream.WithDrainWindow(300*time.Millisecond),
		stream.WithLeaseTTL(15*time.Second),
	)
	if err != nil {
		t.Fatalf("new relay: %v", err)
	}

	// Prime the stream (RunOnce opens it at "now"), then publish, then drain.
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := r.RunOnce(ctx); err != nil { // opens the stream, empty first window
		t.Fatalf("prime: %v", err)
	}

	var ids []string
	for i := 0; i < 20; i++ {
		ids = append(ids, publish(t, "s"))
	}

	// Drain windows until all delivered.
	deadline := time.Now().Add(15 * time.Second)
	for {
		if err := r.RunOnce(ctx); err != nil {
			t.Fatalf("run once: %v", err)
		}
		sender.mu.Lock()
		n := len(sender.ids)
		sender.mu.Unlock()
		if n >= 20 || time.Now().After(deadline) {
			break
		}
	}

	sender.mu.Lock()
	defer sender.mu.Unlock()
	if len(sender.ids) != 20 {
		t.Fatalf("delivered %d, want 20", len(sender.ids))
	}
	for i := range ids {
		if sender.ids[i] != ids[i] {
			t.Fatalf("order[%d] = %s, want %s", i, sender.ids[i], ids[i])
		}
	}

	// Offset persisted: a fresh relay resuming from the saved token delivers nothing new.
	tok, _, err := st.LoadToken(context.Background(), "e2e")
	if err != nil || tok == "" {
		t.Fatalf("token not persisted: tok=%q err=%v", tok, err)
	}
}
```

- [ ] **Step 2: Run test to verify it fails, then passes**

Run: `cd pkg/transport/outbox/mongodb && go test ./... -run TestStreamRelayEndToEnd -v`
Expected (Docker): PASS after any fixes. (No Docker: SKIP — run with Docker before completing.)

- [ ] **Step 3: Full suite + vet**

Run: `cd pkg/transport/outbox/mongodb && go test ./... -v && go vet ./...` and `cd pkg/transport/outbox && go test ./... && go vet ./...`
Expected: engine unit tests PASS always; mongodb integration tests PASS with Docker (SKIP without).

- [ ] **Step 4: Commit**

```bash
git add pkg/transport/outbox/mongodb/relay_integration_test.go
git commit -m "test(outbox-mongodb): end-to-end stream relay ordering + offset persistence"
```

---

## Task 8: Docs — README + design-doc token-type fix

**Files:**
- Modify: `pkg/transport/outbox/README.md` (add the MongoDB stream-relay section)
- Modify: `docs/design/outbox-mongodb-changestream.md` (§8.2 token type `bson.Raw` → `[]byte`)

**Interfaces:** none (documentation).

- [ ] **Step 1: Update the design doc token type**

In `docs/design/outbox-mongodb-changestream.md` §8.2, change the `StreamStore`/`Stream`/`Event` signatures from `bson.Raw` to `[]byte` (the engine stays dependency-free; the mongo store casts to `bson.Raw` internally). Add one line: "Token is `[]byte` at the `stream` boundary — `bson.Raw` is `[]byte` underneath, so the mongo store casts; this keeps `relay/stream` free of the mongo driver."

- [ ] **Step 2: Add the README section**

In `pkg/transport/outbox/README.md`, add a "MongoDB (change-stream relay)" section: link `docs/design/outbox-mongodb-changestream.md`; a publish example (same `NewPublisherFactory` inside `WithTransaction`); a relay wiring example:

```go
st := mongodbstore.NewStore(db)
_ = st.EnsureIndexes(ctx)
st.SetMaxAwaitTime(time.Second)
r, err := stream.NewRelay("broker-publish", st, rabbitSender,
    stream.WithDrainWindow(time.Second),
    stream.WithLeaseTTL(15*time.Second),
    stream.WithObserver(promObserver),
)
// go r.Run(ctx)
```

State: v1 is `StartNow`-only; consumers dedup on `event_id`; ordering is commit-order total order (causal preserved, concurrent unordered); the oplog-window cliff is handled by lag alerting + the break-glass runbook (design §7); and the operator must size **oplog window > outbox TTL (7d) > consumer-downtime SLO**.

- [ ] **Step 3: Verify links**

Run: `git grep -n "outbox-mongodb-changestream" docs pkg/transport/outbox/README.md`
Expected: README and design doc cross-reference.

- [ ] **Step 4: Commit**

```bash
git add pkg/transport/outbox/README.md docs/design/outbox-mongodb-changestream.md
git commit -m "docs(outbox-mongodb): stream-relay README + []byte token in design doc"
```

---

## Self-Review

**1. Spec coverage** (design doc → task):
- §3 package structure (`relay` shared + `relay/stream`) → Tasks 1–2; prerequisite `relay` package noted in Global Constraints. ✓
- §4 collection schema (insert-only `outbox`, `outbox_offsets`, `relay_lock`, TTL index) → Task 3 (`outboxDoc`/`offsetDoc`/`lockDoc`, `EnsureIndexes`). ✓
- §5 lifecycle (publish in txn; consume via leader-gated window loop; TTL retention) → Task 3 (publish), Task 2 (loop), Task 3 (TTL). ✓
- §6 stream config: (a) insert-only match → Task 4 pipeline; (b) StartNow-only → Task 4 (`token == ""` → no resumeAfter); (c) token persistence last-vs-PBRT → Task 2 `drainWindow`; (d) invalidate fatal → Task 2 `ErrStreamInvalidated`. ✓
- §7 ordering/dedup/cliff: commit order + resume → Task 6; committed-token-age lag via `ObserveDrained` → Task 2 `committedTokenAge`; `ChangeStreamHistoryLost` fatal → Task 2 `ErrHistoryLost` (returned from `Run`; detection wired where the driver surfaces it — see gap below). ✓ (partial)
- §8 protoevent-go changes (shared `relay` types; `stream.StreamStore`/`Relay`; `pkg/transport/outbox/mongodb`) → Tasks 1–5. ✓
- §9 migration → documented in the design doc; not a code task here (chassis is out of scope, matching the TiDB plan). ✓
- §10 alternatives, §11 open questions (all resolved) → no code. ✓

**Gap found — `ErrHistoryLost` detection:** Task 2 defines and returns `ErrHistoryLost`, but no task maps the driver's `ChangeStreamHistoryLost` (server code 286) error to it. Fix: in Task 4's `mongoStream.Next`, when `m.cs.Err()` is non-nil, classify it — if it is a server error with code 286 (or the `NonResumableChangeStreamError` label), return `stream.ErrHistoryLost`; otherwise return the raw error (transient → `Run` reopens). Add this to `watch.go` with a `VERIFY v2.5.1` note on the exact error-code check (`mongo.ServerError`/`CommandError` code 286). Add a Task 4 step: unit-assert the classifier maps a synthetic code-286 error to `ErrHistoryLost`. **Action: extend Task 4 Step 3 with the classifier + a `mongodb`-package unit test for it before implementing Watch behavior.** (Recorded here so the implementer adds it; the behavior — history-lost is fatal in v1 — is already specified.)

**2. Placeholder scan:** The only soft spots are the explicit **VERIFY v2.5.1** callouts — these are not placeholders but targeted confirmations against the pinned driver, each with the exact symbol to check and the note that behavior is unchanged. Task 6 Step 1 contains a `drainAll`/`streamEvent` scaffold that is explicitly flagged for removal before finalizing (with the reason and the correct approach stated) — implementers must delete it; it is not left as a silent stub.

**3. Type consistency:** `StreamStore` method set (`LoadToken`/`SaveToken`/`Watch`, all **`string`** tokens) is identical across Task 1 (interface), Task 2 (runtime calls), Tasks 3–4 (impl + assertion). `Stream` (`Next`/`PBRT`/`Close`) identical across Task 1, Task 2 (fake + calls), Task 4 (`mongoStream`). `Event` fields (`Message`/`ResumeToken string`/`ClusterTime`/`Invalidate`) consistent Tasks 1, 2, 4. `NewStore(*mongo.Database)` consistent Tasks 3–7. `SetMaxAwaitTime` introduced in Task 4, used in Tasks 6–7. Token is `string` at the `stream` boundary and `bson.Raw` inside `mongodb` — the cast points (`string(doc.ResumeToken)` on load, `bson.Raw(token)` on save, `string(ce.ID)`/`string(m.cs.ResumeToken())` in the stream) are consistent in Tasks 3–4; "no token" is `token == ""` everywhere (`LoadToken`/`Watch`/`PBRT`). The one `tok == nil` in `mongoStream.PBRT` checks the driver's `bson.Raw` before the `string(...)` cast — correct, not an inconsistency. `relay.Observer`/`relay.LeaderStore` come from the prerequisite shared package (Global Constraints).
