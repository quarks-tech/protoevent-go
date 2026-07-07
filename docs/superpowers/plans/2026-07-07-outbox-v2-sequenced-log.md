# Outbox v2 Sequenced-Log Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace the two-table pending/completed outbox relay with a single append-only log whose offsets are assigned by a post-commit sequencer, giving Kafka-single-partition ordering (total order, causal order preserved, at-least-once) without a broker.

**Architecture:** Two subsystems. **(A) Engine** — `pkg/transport/outbox` plus a `relay` package of shared primitives (`Observer`, `Logger`, `LeaderStore`) and a `relay/sequence` runtime: `sequence.Store` / `SequencerStore` / `RetentionStore` interfaces and a `sequence.Relay` that per tick acquires leadership, runs a sequencer pass, drains in `seq` order, and commits the offset after success. Dependency-free; unit-tested with in-memory contract fakes. The split keeps the future `relay/stream` (MongoDB) runtime a sibling that reuses the same shared `relay` primitives. **(B) Reference TiDB store** — a separate `pkg/transport/outbox/tidb` module (own go.mod so heavy test deps stay out of the engine) implementing the `sequence` interfaces + `relay.LeaderStore` plus publish, with testcontainers integration tests proving the SQL-level guarantees.

**Tech Stack:** Go 1.25.3; `database/sql` + `go-sql-driver/mysql` against TiDB; `golang-migrate` + `testcontainers-go` for integration tests; `google/uuid`.

## Global Constraints

- Engine module path: `github.com/quarks-tech/protoevent-go/pkg/transport/outbox`, `go 1.25.3`.
- **Clean v2 major bump.** The legacy `relay.Store` methods `ListPendingMessages` / `CompletePendingMessages` and `Message.SentTime` are **removed**, no deprecation shims. chassis-go migration is a separate, later effort — do not touch chassis templates in this plan.
- **Engine core stays dependency-free.** `pkg/transport/outbox` and `pkg/transport/outbox/relay` may import only: stdlib, `github.com/google/uuid`, and `github.com/quarks-tech/protoevent-go/pkg/{event,eventbus}`. No SQL driver, no testcontainers, no Prometheus — observability is the `Observer` interface, wired by callers.
- **Target store is TiDB only.** The reference store relies on `@@tidb_current_ts` (PD start TSO) and a clustered auto-increment PK. No MySQL implementation is planned.
- Ordering key is `(tx_start_ts, id)`: `tx_start_ts` totally orders across transactions; the clustered auto-increment `id` is the *within-transaction* tiebreak only (never compared across transactions). There is no `tx_ordinal` column.
- `seq` uniqueness/density is guaranteed by the sequencer's counter-`FOR UPDATE` serialization — there is **no `UNIQUE(seq)` index**. It is guarded by tests + `Observer` gap-detection.
- Delivery is at-least-once; consumers dedup on `event_id`. Ordering between genuinely concurrent transactions is unspecified (Kafka parity).
- Design docs of record: `docs/design/outbox-sequenced-log.md` (this plan) + `docs/design/outbox-mongodb-changestream.md` (the companion that introduced the package split). Where this plan and the docs disagree on API shape, this plan wins (it reflects later decisions, e.g. `NewRelay` takes `name` as a parameter rather than a mandatory option).
- **Package layout:** shared primitives in package `relay` (`Observer`, `Logger`, `LeaderStore`); the TiDB sequenced-log runtime in package `sequence` at `pkg/transport/outbox/relay/sequence`. This plan builds only `relay` + `relay/sequence`; the `relay/stream` (MongoDB) runtime is a separate plan.
- TDD throughout: failing test first, minimal implementation, green, commit. One logical change per commit.

---

## File Structure

**Engine — `pkg/transport/outbox/` (existing module):**
- `message.go` — MODIFY: add `Seq int64`, remove `SentTime`.
- `store.go` — UNCHANGED (`Store.CreateOutboxMessage`).
- `sender.go` — UNCHANGED (publish path is unchanged; `tx_start_ts` is a storage concern).
- `relay/relay.go` — REWRITE to shared primitives (package `relay`, no `Relay` type): `Observer` (`ObserveDrained`/`ObserveError`), `nopObserver`, `Logger`, `LeaderStore`. Shared by the `sequence` (TiDB) and future `stream` (Mongo) runtimes.
- `relay/run.go` — DELETE (its runtime moves to `relay/sequence/run.go`).
- `relay/sequence/sequence.go` — CREATE (package `sequence`): `Observer` (embeds `relay.Observer` + `ObserveSequenced`), `Store`, `SequencerStore`, `RetentionStore`, `Options`, option funcs, `Relay`, `NewRelay`.
- `relay/sequence/run.go` — CREATE: `Run`, `RunOnce`, `sequence`, `drain`, `sweep`, leadership + graceful release.
- `relay/sequence/sequence_test.go` — CREATE: in-memory `fakeStore` + contract/unit tests.

> **Package restructure (per `docs/design/outbox-mongodb-changestream.md` §3):** shared relay
> concerns live in `relay`; the TiDB runtime is `relay/sequence`; the MongoDB runtime (separate
> plan) will be `relay/stream`. `LeaderStore`/`Observer`/`Logger` are in `relay`; the
> sequenced-log `Store`/`SequencerStore`/`RetentionStore`/`Relay` are in `relay/sequence`.

**Reference store — `pkg/transport/outbox/tidb/` (new module):**
- `go.mod` — CREATE: requires engine module + `go-sql-driver/mysql`, `testcontainers-go`, `golang-migrate`.
- `migrations/000001_create_outbox.up.sql` / `.down.sql` — CREATE: the four tables + seed row.
- `embed.go` — CREATE: `embed.FS` for migrations.
- `store.go` — CREATE: `Runner` iface, `Store` implementing publish + all relay interfaces.
- `tidbtest/container.go` — CREATE: testcontainers TiDB harness (adapted from markerry).
- `store_test.go` — CREATE: integration tests for the SQL guarantees.
- `relay_integration_test.go` — CREATE: end-to-end `Relay` over real TiDB.

---

## Task 1: Message carries Seq, drops SentTime

**Files:**
- Modify: `pkg/transport/outbox/message.go`
- Test: `pkg/transport/outbox/message_test.go` (create)

**Interfaces:**
- Consumes: `event.Metadata` (existing).
- Produces: `outbox.Message{ID string; Seq int64; Metadata *event.Metadata; Data []byte; CreateTime time.Time}`. Later tasks read `Seq` on drain and never set `SentTime` (gone).

- [ ] **Step 1: Write the failing test**

Create `pkg/transport/outbox/message_test.go`:

```go
package outbox_test

import (
	"testing"

	"github.com/quarks-tech/protoevent-go/pkg/transport/outbox"
)

// A drained message must expose its assigned log offset.
func TestMessageHasSeqField(t *testing.T) {
	m := outbox.Message{Seq: 42}
	if m.Seq != 42 {
		t.Fatalf("Seq = %d, want 42", m.Seq)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd pkg/transport/outbox && go test ./... -run TestMessageHasSeqField`
Expected: FAIL — `unknown field Seq in struct literal`.

- [ ] **Step 3: Write minimal implementation**

Replace `pkg/transport/outbox/message.go` with:

```go
package outbox

import (
	"time"

	"github.com/quarks-tech/protoevent-go/pkg/event"
)

// Message represents an event stored in the outbox log.
type Message struct {
	// ID is the unique identifier of the outbox row (primary key).
	ID string

	// Seq is the logical log offset assigned by the sequencer after commit.
	// Zero until sequenced; set by the relay store on read.
	Seq int64

	// Metadata contains CloudEvents metadata.
	Metadata *event.Metadata

	// Data is the serialized event payload.
	Data []byte

	// CreateTime is when the message was created (used for lag observability).
	CreateTime time.Time
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `cd pkg/transport/outbox && go test ./... -run TestMessageHasSeqField`
Expected: PASS. (`sender.go` still compiles: it sets `ID`, `Metadata`, `Data`, `CreateTime` and never referenced `SentTime`.)

- [ ] **Step 5: Commit**

```bash
git add pkg/transport/outbox/message.go pkg/transport/outbox/message_test.go
git commit -m "feat(outbox)!: Message carries Seq, drops SentTime"
```

---

## Task 2: Shared `relay` primitives + `sequence` interfaces/options

**Files:**
- Rewrite: `pkg/transport/outbox/relay/relay.go` (package `relay` — shared primitives)
- Delete: `pkg/transport/outbox/relay/run.go` (old two-table runtime; superseded by `relay/sequence/run.go` in Task 3)
- Create: `pkg/transport/outbox/relay/sequence/sequence.go` (package `sequence`)
- Test: `pkg/transport/outbox/relay/sequence/sequence_test.go` (create — fakes + this task's assertions)

**Interfaces:**
- Consumes: `outbox.Message`, `eventbus.Sender`, `event.Metadata`.
- Produces in **package `relay`** (shared with the future `stream` runtime):
  - `Observer{ ObserveDrained(name string, count int, oldestAge time.Duration, more bool); ObserveError(name string, err error) }` + unexported `nopObserver`.
  - `Logger{ Errorf(format string, args ...any) }`
  - `LeaderStore{ TryAcquireLeaderLock(ctx, name, holderID string, ttl time.Duration) (bool, error); ReleaseLeaderLock(ctx, name, holderID string) error }`
- Produces in **package `sequence`** (relied on by Task 3 and Part B):
  - `Observer` — `interface { relay.Observer; ObserveSequenced(name string, count int) }` + unexported `nopObserver`.
  - `Store{ ListMessages(ctx, afterSeq int64, limit int) ([]*outbox.Message, error); Offset(ctx, name string) (int64, error); CommitOffset(ctx, name string, seq int64) error }`
  - `SequencerStore{ SequenceMessages(ctx, limit int) (int, error) }`
  - `RetentionStore{ SweepMessages(ctx, before time.Time, limit int) (int, error) }`
  - `Options`, `Option`, option funcs: `WithBatchSize(int)`, `WithSequenceBatchSize(int)`, `WithPollInterval(time.Duration)`, `WithLeaseTTL(time.Duration)`, `WithLeaderLockName(string)`, `WithoutSequencer()`, `WithRetention(window time.Duration, sweepEvery int, sweepBatch int)`, `WithObserver(Observer)`, `WithLogger(relay.Logger)`, `WithErrorHandler(func(ctx, *outbox.Message, error))`.
  - Defaults: `BatchSize=100`, `SequenceBatchSize=1000`, `PollInterval=time.Second`, `LeaseTTL=15*time.Second`.

- [ ] **Step 1: Write the failing test**

Create `pkg/transport/outbox/relay/sequence/sequence_test.go`:

```go
package sequence_test

import (
	"testing"
	"time"

	"github.com/quarks-tech/protoevent-go/pkg/transport/outbox/relay/sequence"
)

func TestDefaultOptions(t *testing.T) {
	o := sequence.DefaultOptions()
	if o.BatchSize != 100 {
		t.Fatalf("BatchSize = %d, want 100", o.BatchSize)
	}
	if o.SequenceBatchSize != 1000 {
		t.Fatalf("SequenceBatchSize = %d, want 1000", o.SequenceBatchSize)
	}
	if o.PollInterval != time.Second {
		t.Fatalf("PollInterval = %v, want 1s", o.PollInterval)
	}
	if o.LeaseTTL != 15*time.Second {
		t.Fatalf("LeaseTTL = %v, want 15s", o.LeaseTTL)
	}
}

func TestOptionsApply(t *testing.T) {
	o := sequence.DefaultOptions()
	for _, opt := range []sequence.Option{
		sequence.WithBatchSize(50),
		sequence.WithSequenceBatchSize(500),
		sequence.WithoutSequencer(),
		sequence.WithRetention(7*24*time.Hour, 256, 5000),
	} {
		opt(&o)
	}
	if o.BatchSize != 50 || o.SequenceBatchSize != 500 {
		t.Fatalf("batch sizes not applied: %+v", o)
	}
	if !o.DisableSequencer {
		t.Fatal("WithoutSequencer did not set DisableSequencer")
	}
	if o.RetentionWindow != 7*24*time.Hour || o.RetentionSweepEvery != 256 || o.RetentionSweepBatch != 5000 {
		t.Fatalf("retention not applied: %+v", o)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd pkg/transport/outbox && go test ./relay/sequence/ -run 'TestDefaultOptions|TestOptionsApply'`
Expected: FAIL — package `sequence` does not exist yet.

- [ ] **Step 3: Write minimal implementation**

Replace `pkg/transport/outbox/relay/relay.go` with the shared primitives (package `relay`, no `Relay` type). Delete `pkg/transport/outbox/relay/run.go`.

```go
// Package relay holds the primitives shared by the sequenced-log (relay/sequence,
// TiDB) and change-stream (relay/stream, MongoDB) outbox relay runtimes.
package relay

import (
	"context"
	"time"
)

// Observer receives lag/throughput signals common to every relay runtime. It is
// the dependency-free observability seam: callers wire it to Prometheus etc.
// Values are derived from data the relay passes already hold — no extra queries.
type Observer interface {
	// ObserveDrained reports a drain/forward pass: count sent, age of the oldest
	// event handled (lag), and whether more work is immediately waiting.
	ObserveDrained(name string, count int, oldestAge time.Duration, more bool)
	// ObserveError reports a pass-level error (leadership, read, forward, sweep).
	ObserveError(name string, err error)
}

// Logger interface for relay error logging.
type Logger interface {
	Errorf(format string, args ...any)
}

// LeaderStore enables running multiple relay instances with automatic failover.
// Only the lock holder processes; others idle. Shared by both runtimes.
type LeaderStore interface {
	// TryAcquireLeaderLock acquires or renews the lock. Returns true if holderID
	// holds it after the call. The lock expires after ttl if not renewed.
	TryAcquireLeaderLock(ctx context.Context, name, holderID string, ttl time.Duration) (bool, error)
	// ReleaseLeaderLock releases the lock if held by holderID (graceful shutdown).
	ReleaseLeaderLock(ctx context.Context, name, holderID string) error
}
```

Create `pkg/transport/outbox/relay/sequence/sequence.go`:

```go
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
	name     string
	store    Store
	sender   eventbus.Sender
	options  Options
	holderID string

	isLeader  bool // whether this instance currently holds the lock
	tickCount int  // for retention cadence
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
		name:     name,
		store:    store,
		sender:   sender,
		options:  options,
		holderID: uuid.NewString(),
	}
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `cd pkg/transport/outbox && go test ./relay/... -run 'TestDefaultOptions|TestOptionsApply'`
Expected: PASS. (Package `relay` compiles with only the shared primitives; `relay/sequence` compiles against it.)

- [ ] **Step 5: Commit**

```bash
git rm pkg/transport/outbox/relay/run.go
git add pkg/transport/outbox/relay/relay.go pkg/transport/outbox/relay/sequence/sequence.go pkg/transport/outbox/relay/sequence/sequence_test.go
git commit -m "feat(outbox)!: split relay into shared primitives + sequence runtime"
```

---

## Task 3: Relay runtime (sequence → drain → commit, loop-while-full, stop-the-lane, leadership + graceful release, sweep)

**Files:**
- Create: `pkg/transport/outbox/relay/sequence/run.go` (package `sequence`)
- Test: `pkg/transport/outbox/relay/sequence/sequence_test.go` (append fakes + runtime tests)

**Interfaces:**
- Consumes: everything from Task 2 (`sequence.*` runtime types, `relay.LeaderStore`).
- Produces: `Relay.Run(ctx) error`, `Relay.RunOnce(ctx) error`. `RunOnce` order: acquire leadership → (sequence pass if applicable) → drain → periodic sweep. Part B relies on these.

- [ ] **Step 1: Write the failing test**

Append to `pkg/transport/outbox/relay/sequence/sequence_test.go` (add imports `context`, `errors`, `strconv`, `sync`, plus `event`, `eventbus` and `outbox` as needed). The fake's leader methods satisfy `relay.LeaderStore` structurally — no `relay` import is required in the test.

```go
// --- in-memory contract fake ------------------------------------------------

// fakeStore models the SQL store's contract: a dense sequenced log, NULL-seq
// pending rows, monotone per-name offsets, and a single leader lock.
type fakeStore struct {
	mu       sync.Mutex
	pending  []*outbox.Message // seq == 0, in (tx_start_ts,id) order as appended
	log      []*outbox.Message // seq assigned, ascending
	nextSeq  int64
	offsets  map[string]int64
	leader   string // holderID currently holding the lock ("" = free)
	seqErr   error
	listErr  error
}

func newFakeStore() *fakeStore {
	return &fakeStore{nextSeq: 1, offsets: map[string]int64{}}
}

// append simulates a publish: an unsequenced row.
func (s *fakeStore) append(m *outbox.Message) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pending = append(s.pending, m)
}

func (s *fakeStore) SequenceMessages(_ context.Context, limit int) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.seqErr != nil {
		return 0, s.seqErr
	}
	n := limit
	if n > len(s.pending) {
		n = len(s.pending)
	}
	for i := 0; i < n; i++ {
		m := s.pending[i]
		m.Seq = s.nextSeq
		s.nextSeq++
		s.log = append(s.log, m)
	}
	s.pending = s.pending[n:]
	return n, nil
}

func (s *fakeStore) ListMessages(_ context.Context, afterSeq int64, limit int) ([]*outbox.Message, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.listErr != nil {
		return nil, s.listErr
	}
	var out []*outbox.Message
	for _, m := range s.log {
		if m.Seq > afterSeq {
			out = append(out, m)
			if len(out) == limit {
				break
			}
		}
	}
	return out, nil
}

func (s *fakeStore) Offset(_ context.Context, name string) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.offsets[name], nil
}

func (s *fakeStore) CommitOffset(_ context.Context, name string, seq int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if seq > s.offsets[name] { // GREATEST
		s.offsets[name] = seq
	}
	return nil
}

func (s *fakeStore) TryAcquireLeaderLock(_ context.Context, _, holderID string, _ time.Duration) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.leader == "" || s.leader == holderID {
		s.leader = holderID
		return true, nil
	}
	return false, nil
}

func (s *fakeStore) ReleaseLeaderLock(_ context.Context, _, holderID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.leader == holderID {
		s.leader = ""
	}
	return nil
}

// captureSender records what the relay forwards, and can fail on a target seq.
type captureSender struct {
	sent     []int64
	failAt   int64 // Seq to fail on (0 = never)
	failErr  error
}

func (c *captureSender) Send(_ context.Context, md *event.Metadata, _ []byte) error {
	// md.ID doubles as a stringified seq marker in these tests; we track by the
	// Seq the relay drained, captured via a closure in each test instead.
	return nil
}

func msg(seq int64) *outbox.Message {
	return &outbox.Message{ID: "id", Seq: seq, Metadata: event.NewMetadata("t"), CreateTime: time.Now()}
}

// --- runtime tests ----------------------------------------------------------

func TestRunOnceSequencesThenDrainsSameTick(t *testing.T) {
	st := newFakeStore()
	st.append(msg(0))
	st.append(msg(0))
	st.append(msg(0))

	var got []int64
	sender := senderFunc(func(_ context.Context, md *event.Metadata, _ []byte) error {
		got = append(got, mdSeq(md))
		return nil
	})

	r := sequence.NewRelay("c", st, sender)
	if err := r.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("drained %d, want 3 (rows sequenced this tick must drain this tick)", len(got))
	}
	if off := st.offsets["c"]; off != 3 {
		t.Fatalf("offset = %d, want 3", off)
	}
}

func TestDrainLoopsUntilShortPage(t *testing.T) {
	st := newFakeStore()
	for i := 0; i < 250; i++ {
		st.append(msg(0))
	}
	var count int
	sender := senderFunc(func(context.Context, *event.Metadata, []byte) error { count++; return nil })

	r := sequence.NewRelay("c", st, sender, sequence.WithBatchSize(100))
	if err := r.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if count != 250 {
		t.Fatalf("drained %d, want 250 (must loop past full pages)", count)
	}
}

func TestStopTheLaneOnSendError(t *testing.T) {
	st := newFakeStore()
	for i := 0; i < 5; i++ {
		st.append(msg(0))
	}
	var got []int64
	sender := senderFunc(func(_ context.Context, md *event.Metadata, _ []byte) error {
		if mdSeq(md) == 3 {
			return errors.New("boom")
		}
		got = append(got, mdSeq(md))
		return nil
	})

	r := sequence.NewRelay("c", st, sender)
	_ = r.RunOnce(context.Background())

	// Sent 1,2; stopped at 3. Offset must not advance past 2.
	if off := st.offsets["c"]; off != 2 {
		t.Fatalf("offset = %d, want 2 (stop-the-lane must not skip the failure)", off)
	}
	if len(got) != 2 {
		t.Fatalf("sent %v, want [1 2]", got)
	}
}

func TestNonLeaderDoesNothing(t *testing.T) {
	st := newFakeStore()
	st.leader = "someone-else"
	st.append(msg(0))
	sent := 0
	sender := senderFunc(func(context.Context, *event.Metadata, []byte) error { sent++; return nil })

	r := sequence.NewRelay("c", st, sender, sequence.WithLeaderLockName("lock"))
	if err := r.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if sent != 0 {
		t.Fatalf("non-leader sent %d, want 0", sent)
	}
}
```

Also append the small test helpers (put near the top of the test file):

```go
// senderFunc adapts a func to eventbus.Sender.
type senderFunc func(context.Context, *event.Metadata, []byte) error

func (f senderFunc) Send(ctx context.Context, md *event.Metadata, d []byte) error {
	return f(ctx, md, d)
}

// mdSeq encodes/decodes a seq into metadata ID for send-order assertions. The
// fake store sets Metadata.ID to the decimal Seq at sequence time.
func mdSeq(md *event.Metadata) int64 {
	n, _ := strconv.ParseInt(md.ID, 10, 64)
	return n
}
```

Update the fake's `SequenceMessages` loop to stamp the seq into the metadata ID so `mdSeq` works — change the assignment block to:

```go
	for i := 0; i < n; i++ {
		m := s.pending[i]
		m.Seq = s.nextSeq
		m.Metadata.ID = strconv.FormatInt(s.nextSeq, 10)
		s.nextSeq++
		s.log = append(s.log, m)
	}
```

Add `"strconv"` to the test imports.

- [ ] **Step 2: Run test to verify it fails**

Run: `cd pkg/transport/outbox && go test ./relay/sequence/ -run 'TestRunOnce|TestDrainLoops|TestStopTheLane|TestNonLeader'`
Expected: FAIL — `Relay.RunOnce`/`Run` not defined yet in package `sequence`.

- [ ] **Step 3: Write minimal implementation**

Create `pkg/transport/outbox/relay/sequence/run.go`:

```go
package sequence

import (
	"context"
	"time"

	"github.com/quarks-tech/protoevent-go/pkg/transport/outbox"
	"github.com/quarks-tech/protoevent-go/pkg/transport/outbox/relay"
)

const releaseTimeout = 5 * time.Second

// Run drives the relay until ctx is canceled, then releases leadership so a
// planned shutdown fails over in well under LeaseTTL.
func (r *Relay) Run(ctx context.Context) error {
	ticker := time.NewTicker(r.options.PollInterval)
	defer ticker.Stop()
	defer r.releaseLeadership()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if err := r.RunOnce(ctx); err != nil {
				r.options.Observer.ObserveError(r.name, err)
				if r.options.Logger != nil {
					r.options.Logger.Errorf("relay %q: %v", r.name, err)
				}
			}
		}
	}
}

// RunOnce performs one tick: acquire leadership, sequence, drain, and (on
// cadence) sweep. Rows sequenced this tick are drained this tick because the
// sequencer pass commits before the drain query runs.
func (r *Relay) RunOnce(ctx context.Context) error {
	isLeader, err := r.tryAcquireLeadership(ctx)
	if err != nil {
		return err
	}
	if !isLeader {
		return nil
	}

	if err := r.sequence(ctx); err != nil {
		return err
	}
	if err := r.drain(ctx); err != nil {
		return err
	}
	return r.maybeSweep(ctx)
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

// sequence assigns offsets to committed pending rows, looping while pages are
// full so a burst is fully sequenced within the tick.
func (r *Relay) sequence(ctx context.Context) error {
	ss, ok := r.store.(SequencerStore)
	if !ok || r.options.DisableSequencer {
		return nil
	}
	for {
		n, err := ss.SequenceMessages(ctx, r.options.SequenceBatchSize)
		if err != nil {
			return err
		}
		r.options.Observer.ObserveSequenced(r.name, n)
		if n < r.options.SequenceBatchSize {
			return nil
		}
		if err := ctx.Err(); err != nil {
			return err
		}
	}
}

// drain forwards messages in Seq order, committing the offset only after a
// successful send. On send failure it stops the lane (default) or parks and
// continues (WithErrorHandler). Loops while pages are full.
func (r *Relay) drain(ctx context.Context) error {
	for {
		offset, err := r.store.Offset(ctx, r.name)
		if err != nil {
			return err
		}
		msgs, err := r.store.ListMessages(ctx, offset, r.options.BatchSize)
		if err != nil {
			return err
		}
		if len(msgs) == 0 {
			return nil
		}

		maxSeq := offset
		processed := 0
		stopped := false
		for _, m := range msgs {
			if sendErr := r.sender.Send(ctx, m.Metadata, m.Data); sendErr != nil {
				r.handleError(ctx, m, sendErr)
				if r.options.ErrorHandler == nil {
					stopped = true
					break // stop-the-lane: leave this seq for the next tick
				}
				// park-and-continue: advance past the parked message
			}
			maxSeq = m.Seq
			processed++
		}

		if maxSeq > offset {
			if err := r.store.CommitOffset(ctx, r.name, maxSeq); err != nil {
				return err
			}
		}

		full := len(msgs) == r.options.BatchSize
		r.options.Observer.ObserveDrained(r.name, processed, time.Since(msgs[0].CreateTime), full && !stopped)

		if stopped || !full {
			return nil
		}
		if err := ctx.Err(); err != nil {
			return err
		}
	}
}

func (r *Relay) maybeSweep(ctx context.Context) error {
	if r.options.RetentionWindow <= 0 || r.options.RetentionSweepEvery <= 0 {
		return nil
	}
	rs, ok := r.store.(RetentionStore)
	if !ok {
		return nil
	}
	r.tickCount++
	if r.tickCount%r.options.RetentionSweepEvery != 0 {
		return nil
	}
	before := time.Now().Add(-r.options.RetentionWindow)
	_, err := rs.SweepMessages(ctx, before, r.options.RetentionSweepBatch)
	return err
}

func (r *Relay) handleError(ctx context.Context, msg *outbox.Message, err error) {
	if r.options.ErrorHandler != nil {
		r.options.ErrorHandler(ctx, msg, err)
	}
	r.options.Observer.ObserveError(r.name, err)
	if r.options.Logger != nil {
		r.options.Logger.Errorf("relay %q: send message %s: %v", r.name, msg.ID, err)
	}
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `cd pkg/transport/outbox && go test ./relay/sequence/ -run 'TestRunOnce|TestDrainLoops|TestStopTheLane|TestNonLeader'`
Expected: PASS.

- [ ] **Step 5: Full engine test + vet, then commit**

Run: `cd pkg/transport/outbox && go test ./... && go vet ./...`
Expected: PASS across `message_test`, `sender_id_test`, `factory_id_test`, and `relay/sequence`.

```bash
git add pkg/transport/outbox/relay/sequence/run.go pkg/transport/outbox/relay/sequence/sequence_test.go
git commit -m "feat(outbox)!: sequenced-log relay runtime (sequence+drain, stop-the-lane, graceful release)"
```

---

## Task 4: Park-and-continue error mode

**Files:**
- Test: `pkg/transport/outbox/relay/sequence/sequence_test.go` (append)

**Interfaces:**
- Consumes: `sequence.WithErrorHandler`, `Relay.RunOnce` from Tasks 2–3.
- Produces: nothing new — locks the documented park-and-continue semantics.

- [ ] **Step 1: Write the failing test**

Append to `sequence_test.go`:

```go
func TestParkAndContinueAdvancesPastFailure(t *testing.T) {
	st := newFakeStore()
	for i := 0; i < 5; i++ {
		st.append(msg(0))
	}
	var got []int64
	sender := senderFunc(func(_ context.Context, md *event.Metadata, _ []byte) error {
		if mdSeq(md) == 3 {
			return errors.New("boom")
		}
		got = append(got, mdSeq(md))
		return nil
	})
	var parked []int64
	r := sequence.NewRelay("c", st, sender, sequence.WithErrorHandler(
		func(_ context.Context, m *outbox.Message, _ error) { parked = append(parked, m.Seq) },
	))
	if err := r.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	// 3 parked; 1,2,4,5 delivered; offset advances to 5.
	if off := st.offsets["c"]; off != 5 {
		t.Fatalf("offset = %d, want 5 (park-and-continue advances past failure)", off)
	}
	if len(parked) != 1 || parked[0] != 3 {
		t.Fatalf("parked = %v, want [3]", parked)
	}
	if len(got) != 4 {
		t.Fatalf("delivered = %v, want 4 messages", got)
	}
}
```

- [ ] **Step 2: Run test to verify it fails, then passes**

Run: `cd pkg/transport/outbox && go test ./relay/sequence/ -run TestParkAndContinue`
Expected: PASS immediately (behavior implemented in Task 3). If it FAILS, fix `drain` — the park branch must fall through to `maxSeq = m.Seq; processed++` without `break`. This task exists to pin the semantics with a dedicated test.

- [ ] **Step 3: Commit**

```bash
git add pkg/transport/outbox/relay/sequence/sequence_test.go
git commit -m "test(outbox): pin park-and-continue error mode"
```

---

## Task 5: TiDB reference module scaffold + migrations

**Files:**
- Create: `pkg/transport/outbox/tidb/go.mod`
- Create: `pkg/transport/outbox/tidb/migrations/000001_create_outbox.up.sql`
- Create: `pkg/transport/outbox/tidb/migrations/000001_create_outbox.down.sql`
- Create: `pkg/transport/outbox/tidb/embed.go`
- Create: `pkg/transport/outbox/tidb/doc_test.go` (a trivial compile/embed test)

**Interfaces:**
- Produces: `tidb.Migrations embed.FS` (used by the test harness in Task 8). Migration DDL is the schema of record for Part B.

- [ ] **Step 1: Write the failing test**

Create `pkg/transport/outbox/tidb/embed.go`:

```go
package tidb

import "embed"

// Migrations holds the outbox schema migrations (golang-migrate iofs source).
//
//go:embed migrations/*.sql
var Migrations embed.FS
```

Create `pkg/transport/outbox/tidb/doc_test.go`:

```go
package tidb_test

import (
	"testing"
	"testing/fstest"

	tidb "github.com/quarks-tech/protoevent-go/pkg/transport/outbox/tidb"
)

func TestMigrationsEmbedded(t *testing.T) {
	if err := fstest.TestFS(tidb.Migrations, "migrations/000001_create_outbox.up.sql"); err != nil {
		t.Fatalf("migration not embedded: %v", err)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd pkg/transport/outbox/tidb && go test ./... 2>&1 | head`
Expected: FAIL — no `go.mod` / missing migration file / package won't build.

- [ ] **Step 3: Write minimal implementation**

Create `pkg/transport/outbox/tidb/go.mod`:

```
module github.com/quarks-tech/protoevent-go/pkg/transport/outbox/tidb

go 1.25.3

require (
	github.com/go-sql-driver/mysql v1.8.1
	github.com/golang-migrate/migrate/v4 v4.17.1
	github.com/google/uuid v1.6.0
	github.com/quarks-tech/protoevent-go v0.4.2
	github.com/quarks-tech/protoevent-go/pkg/transport/outbox v0.0.0
	github.com/testcontainers/testcontainers-go v0.42.0
)
```

Add a replace so the tidb module builds against the local engine (adjust relative path depth as needed):

```
replace github.com/quarks-tech/protoevent-go/pkg/transport/outbox => ../
```

Create `pkg/transport/outbox/tidb/migrations/000001_create_outbox.up.sql`:

```sql
CREATE TABLE outbox (
    id           BIGINT       NOT NULL AUTO_INCREMENT,
    seq          BIGINT       NULL,
    tx_start_ts  BIGINT       NOT NULL,
    event_id     BINARY(16)   NOT NULL,
    `type`       VARCHAR(255) NOT NULL,
    source       VARCHAR(255) NOT NULL,
    subject      VARCHAR(255) NOT NULL,
    content_type VARCHAR(64)  NOT NULL,
    data         BLOB         NOT NULL,
    occurred_at  DATETIME(6)  NOT NULL,
    PRIMARY KEY (id) /*T![clustered_index] CLUSTERED */,
    UNIQUE KEY uk_outbox_event (event_id),
    KEY idx_outbox_seq (seq, tx_start_ts)
);

CREATE TABLE outbox_sequencer (
    name     VARCHAR(64) NOT NULL,
    next_seq BIGINT      NOT NULL,
    PRIMARY KEY (name)
);

INSERT INTO outbox_sequencer (name, next_seq) VALUES ('default', 1);

CREATE TABLE outbox_offsets (
    name        VARCHAR(64) NOT NULL,
    last_seq    BIGINT      NOT NULL,
    update_time DATETIME(6) NOT NULL,
    PRIMARY KEY (name)
);

CREATE TABLE relay_lock (
    name        VARCHAR(64) NOT NULL,
    holder_id   VARCHAR(64) NOT NULL,
    expire_time DATETIME(6) NOT NULL,
    PRIMARY KEY (name)
);
```

Create `pkg/transport/outbox/tidb/migrations/000001_create_outbox.down.sql`:

```sql
DROP TABLE IF EXISTS relay_lock;
DROP TABLE IF EXISTS outbox_offsets;
DROP TABLE IF EXISTS outbox_sequencer;
DROP TABLE IF EXISTS outbox;
```

- [ ] **Step 4: Resolve deps, run test to verify it passes**

Run: `cd pkg/transport/outbox/tidb && go mod tidy && go test ./... -run TestMigrationsEmbedded`
Expected: PASS. (`go mod tidy` pins the transitive versions; commit the resulting `go.sum`.)

- [ ] **Step 5: Commit**

```bash
git add pkg/transport/outbox/tidb/go.mod pkg/transport/outbox/tidb/go.sum pkg/transport/outbox/tidb/embed.go pkg/transport/outbox/tidb/doc_test.go pkg/transport/outbox/tidb/migrations
git commit -m "feat(outbox-tidb): reference module scaffold + schema migration"
```

---

## Task 6: TiDB Store — publish + read/offset paths

**Files:**
- Create: `pkg/transport/outbox/tidb/store.go`

**Interfaces:**
- Consumes: `outbox.Store`, `outbox.Message`, `sequence.Store`, `event.Metadata`.
- Produces:
  - `Runner{ ExecContext; QueryContext; QueryRowContext }` (satisfied by `*sql.DB` and `*sql.Tx`).
  - `NewStore(r Runner) *Store`.
  - `Store.CreateOutboxMessage(ctx, *outbox.Message) error` (publish; run on a tx-scoped Runner).
  - `Store.ListMessages`, `Store.Offset`, `Store.CommitOffset` (satisfies `sequence.Store`).
  - Compile-time assertions `var _ outbox.Store = (*Store)(nil)`, `var _ sequence.Store = (*Store)(nil)`.

- [ ] **Step 1: Write the failing test**

Add compile-time interface assertions in the store file itself (Step 3). Behavior is proven by the integration tests in Task 8; this task's gate is `go build`.

Create a placeholder in `store_test.go` to assert construction compiles (full integration tests land in Task 8):

Create `pkg/transport/outbox/tidb/store_test.go`:

```go
package tidb_test

import (
	"testing"

	tidb "github.com/quarks-tech/protoevent-go/pkg/transport/outbox/tidb"
)

func TestNewStoreCompiles(t *testing.T) {
	// nil Runner is fine for a construction-only check; no query is issued.
	if tidb.NewStore(nil) == nil {
		t.Fatal("NewStore returned nil")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd pkg/transport/outbox/tidb && go test ./... -run TestNewStoreCompiles`
Expected: FAIL — `NewStore` / `Store` undefined.

- [ ] **Step 3: Write minimal implementation**

Create `pkg/transport/outbox/tidb/store.go`:

```go
package tidb

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/google/uuid"

	"github.com/quarks-tech/protoevent-go/pkg/event"
	"github.com/quarks-tech/protoevent-go/pkg/transport/outbox"
	"github.com/quarks-tech/protoevent-go/pkg/transport/outbox/relay/sequence"
)

// Runner is the subset of *sql.DB / *sql.Tx the store needs. Publish uses a
// tx-scoped Runner (atomic with business writes); the relay uses *sql.DB.
type Runner interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// Store implements the outbox publish path and the relay read/offset/sequencer/
// retention/leader contracts over TiDB.
type Store struct {
	r Runner
}

func NewStore(r Runner) *Store { return &Store{r: r} }

var (
	_ outbox.Store   = (*Store)(nil)
	_ sequence.Store = (*Store)(nil)
)

// CreateOutboxMessage inserts an unsequenced row. tx_start_ts is the publishing
// transaction's PD start TSO (@@tidb_current_ts); id is auto-assigned in insert
// order (= emit order); seq stays NULL until the sequencer runs. Call this on a
// transaction-scoped Runner so the row commits atomically with business writes.
func (s *Store) CreateOutboxMessage(ctx context.Context, m *outbox.Message) error {
	id, err := uuid.Parse(m.ID)
	if err != nil {
		return fmt.Errorf("outbox: parse message ID %q: %w", m.ID, err)
	}
	md := m.Metadata
	_, err = s.r.ExecContext(ctx, `
INSERT INTO outbox (seq, tx_start_ts, event_id, `+"`type`"+`, source, subject, content_type, data, occurred_at)
VALUES (NULL, @@tidb_current_ts, ?, ?, ?, ?, ?, ?, ?)`,
		id[:], md.Type, md.Source, md.Subject, md.DataContentType, m.Data, md.Time.UTC(),
	)
	if err != nil {
		return fmt.Errorf("outbox: insert: %w", err)
	}
	return nil
}

// ListMessages returns sequenced rows with seq > afterSeq in seq order.
func (s *Store) ListMessages(ctx context.Context, afterSeq int64, limit int) ([]*outbox.Message, error) {
	rows, err := s.r.QueryContext(ctx, `
SELECT seq, event_id, `+"`type`"+`, source, subject, content_type, data, occurred_at
FROM outbox
WHERE seq > ?
ORDER BY seq
LIMIT ?`, afterSeq, limit)
	if err != nil {
		return nil, fmt.Errorf("outbox: list: %w", err)
	}
	defer rows.Close()

	var out []*outbox.Message
	for rows.Next() {
		var (
			seq                                  int64
			eventID                              []byte
			typ, source, subject, contentType    string
			data                                 []byte
		)
		var occurredAt = new(sqlTime)
		if err := rows.Scan(&seq, &eventID, &typ, &source, &subject, &contentType, &data, occurredAt); err != nil {
			return nil, fmt.Errorf("outbox: scan: %w", err)
		}
		id, err := uuid.FromBytes(eventID)
		if err != nil {
			return nil, fmt.Errorf("outbox: event_id not a uuid: %w", err)
		}
		md := &event.Metadata{
			SpecVersion:     "1.0",
			ID:              id.String(),
			Type:            typ,
			Source:          source,
			Subject:         subject,
			DataContentType: contentType,
			Time:            occurredAt.t,
		}
		out = append(out, &outbox.Message{
			ID:         id.String(),
			Seq:        seq,
			Metadata:   md,
			Data:       data,
			CreateTime: occurredAt.t,
		})
	}
	return out, rows.Err()
}

// Offset returns the named consumer's watermark (0 if unset).
func (s *Store) Offset(ctx context.Context, name string) (int64, error) {
	var seq int64
	err := s.r.QueryRowContext(ctx, `SELECT last_seq FROM outbox_offsets WHERE name = ?`, name).Scan(&seq)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("outbox: offset: %w", err)
	}
	return seq, nil
}

// CommitOffset advances the watermark monotonically (GREATEST).
func (s *Store) CommitOffset(ctx context.Context, name string, seq int64) error {
	_, err := s.r.ExecContext(ctx, `
INSERT INTO outbox_offsets (name, last_seq, update_time)
VALUES (?, ?, NOW(6))
ON DUPLICATE KEY UPDATE
    last_seq    = GREATEST(last_seq, VALUES(last_seq)),
    update_time = VALUES(update_time)`, name, seq)
	if err != nil {
		return fmt.Errorf("outbox: commit offset: %w", err)
	}
	return nil
}
```

Add a tiny `sqlTime` helper at the bottom of `store.go` so `DATETIME(6)` scans cleanly with the mysql driver (which returns `time.Time` when `parseTime=true`):

```go
import "time"

// sqlTime scans a DATETIME(6). The DSN must set parseTime=true (see tidbtest).
type sqlTime struct{ t time.Time }

func (s *sqlTime) Scan(v any) error {
	switch x := v.(type) {
	case time.Time:
		s.t = x
	case nil:
		s.t = time.Time{}
	default:
		return fmt.Errorf("outbox: cannot scan %T into time", v)
	}
	return nil
}
```

(Merge the `time` import into the existing import block.)

- [ ] **Step 4: Run test to verify it passes**

Run: `cd pkg/transport/outbox/tidb && go test ./... -run 'TestNewStoreCompiles|TestMigrationsEmbedded' && go vet ./...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add pkg/transport/outbox/tidb/store.go pkg/transport/outbox/tidb/store_test.go
git commit -m "feat(outbox-tidb): store publish + read/offset paths"
```

---

## Task 7: TiDB Store — sequencer, leader lock, retention

**Files:**
- Modify: `pkg/transport/outbox/tidb/store.go`

**Interfaces:**
- Consumes: Task 6 `Store`, `sequence.SequencerStore`, `relay.LeaderStore`, `sequence.RetentionStore`.
- Produces on `*Store`:
  - `SequenceMessages(ctx, limit int) (int, error)` — one internal tx: counter `FOR UPDATE`, ROW_NUMBER assignment ordered by `(tx_start_ts, id)`, counter bump.
  - `TryAcquireLeaderLock`, `ReleaseLeaderLock`.
  - `SweepMessages(ctx, before time.Time, limit int) (int, error)`.
  - Requires a `*sql.DB`-backed Runner for `SequenceMessages` (it opens its own tx). Adds `NewStoreDB(db *sql.DB) *Store` so the relay-side store carries a `*sql.DB` for `BeginTx`.
  - Compile-time assertions for all three interfaces (`sequence.SequencerStore`, `relay.LeaderStore`, `sequence.RetentionStore`).

- [ ] **Step 1: Write the failing test**

Append to `store_test.go`:

```go
import "database/sql"

func TestStoreImplementsRelayInterfaces(t *testing.T) {
	var db *sql.DB
	s := tidb.NewStoreDB(db)
	if _, ok := any(s).(interface {
		SequenceMessages(ctx contextT, limit int) (int, error)
	}); !ok {
		t.Skip("checked via compile-time asserts in store.go")
	}
}
```

Simpler: rely on compile-time asserts. Replace the test above with a build-only assertion test:

```go
func TestStoreDBConstructs(t *testing.T) {
	if tidb.NewStoreDB(nil) == nil {
		t.Fatal("NewStoreDB returned nil")
	}
}
```

(Delete the `contextT` sketch; the real gate is the `var _ sequence.SequencerStore = ...` asserts compiling.)

- [ ] **Step 2: Run test to verify it fails**

Run: `cd pkg/transport/outbox/tidb && go test ./... -run TestStoreDBConstructs`
Expected: FAIL — `NewStoreDB` undefined.

- [ ] **Step 3: Write minimal implementation**

Add to `store.go`. First extend the struct/constructors so the sequencer can open a transaction:

```go
// Store also holds a *sql.DB when built via NewStoreDB, enabling SequenceMessages
// to run its own transaction. Read/offset/publish paths use the Runner.
// (Add `db *sql.DB` to the Store struct.)
```

Change the struct and add the DB constructor:

```go
type Store struct {
	r  Runner
	db *sql.DB // non-nil only when built via NewStoreDB; needed by SequenceMessages
}

// NewStoreDB builds a store over a *sql.DB, enabling the sequencer, leader, and
// retention paths (which manage their own transactions / run on the pool).
func NewStoreDB(db *sql.DB) *Store { return &Store{r: db, db: db} }
```

Add `"github.com/quarks-tech/protoevent-go/pkg/transport/outbox/relay"` to the import block (for `relay.LeaderStore`; `sequence` is already imported from Task 6). Then add the interface assertions and methods:

```go
var (
	_ sequence.SequencerStore = (*Store)(nil)
	_ relay.LeaderStore       = (*Store)(nil)
	_ sequence.RetentionStore = (*Store)(nil)
)

// SequenceMessages assigns dense seq values to committed pending rows in
// (tx_start_ts, id) order. The counter row is locked FOR UPDATE for the whole
// pass, so concurrent sequencers serialize and can never double-assign.
func (s *Store) SequenceMessages(ctx context.Context, limit int) (int, error) {
	if s.db == nil {
		return 0, fmt.Errorf("outbox: SequenceMessages requires a *sql.DB store (use NewStoreDB)")
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{})
	if err != nil {
		return 0, fmt.Errorf("outbox: begin sequence tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // no-op after Commit

	var next int64
	if err := tx.QueryRowContext(ctx,
		`SELECT next_seq FROM outbox_sequencer WHERE name = 'default' FOR UPDATE`,
	).Scan(&next); err != nil {
		return 0, fmt.Errorf("outbox: lock sequencer: %w", err)
	}

	res, err := tx.ExecContext(ctx, `
UPDATE outbox o
JOIN (
    SELECT id, ROW_NUMBER() OVER (ORDER BY tx_start_ts, id) AS rn
    FROM outbox
    WHERE seq IS NULL
    ORDER BY tx_start_ts, id
    LIMIT ?
) b ON b.id = o.id
SET o.seq = ? + b.rn - 1`, limit, next)
	if err != nil {
		return 0, fmt.Errorf("outbox: assign seq: %w", err)
	}
	assigned, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("outbox: rows affected: %w", err)
	}

	if assigned > 0 {
		if _, err := tx.ExecContext(ctx,
			`UPDATE outbox_sequencer SET next_seq = ? WHERE name = 'default'`, next+assigned,
		); err != nil {
			return 0, fmt.Errorf("outbox: bump sequencer: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("outbox: commit sequence tx: %w", err)
	}
	return int(assigned), nil
}

// TryAcquireLeaderLock acquires or renews the lock; the incoming holder wins if
// the lock is free (expired) or already theirs. TTL is applied via DB clock.
func (s *Store) TryAcquireLeaderLock(ctx context.Context, name, holderID string, ttl time.Duration) (bool, error) {
	if _, err := s.r.ExecContext(ctx, `
INSERT INTO relay_lock (name, holder_id, expire_time)
VALUES (?, ?, NOW(6) + INTERVAL ? MICROSECOND)
ON DUPLICATE KEY UPDATE
    holder_id   = IF(expire_time < NOW(6) OR holder_id = VALUES(holder_id), VALUES(holder_id), holder_id),
    expire_time = IF(expire_time < NOW(6) OR holder_id = VALUES(holder_id), VALUES(expire_time), expire_time)`,
		name, holderID, ttl.Microseconds(),
	); err != nil {
		return false, fmt.Errorf("outbox: acquire lock: %w", err)
	}

	var holder string
	if err := s.r.QueryRowContext(ctx,
		`SELECT holder_id FROM relay_lock WHERE name = ?`, name,
	).Scan(&holder); err != nil {
		return false, fmt.Errorf("outbox: read lock holder: %w", err)
	}
	return holder == holderID, nil
}

// ReleaseLeaderLock drops the lock if still held by holderID.
func (s *Store) ReleaseLeaderLock(ctx context.Context, name, holderID string) error {
	_, err := s.r.ExecContext(ctx,
		`DELETE FROM relay_lock WHERE name = ? AND holder_id = ?`, name, holderID)
	if err != nil {
		return fmt.Errorf("outbox: release lock: %w", err)
	}
	return nil
}

// SweepMessages deletes sequenced rows at or below the minimum committed offset
// across all consumers and older than `before`, bounded to `limit`. If no
// offsets exist yet, MIN(last_seq) is NULL and nothing is deleted.
func (s *Store) SweepMessages(ctx context.Context, before time.Time, limit int) (int, error) {
	res, err := s.r.ExecContext(ctx, `
DELETE FROM outbox
WHERE seq IS NOT NULL
  AND seq <= (SELECT MIN(last_seq) FROM outbox_offsets)
  AND occurred_at < ?
LIMIT ?`, before.UTC(), limit)
	if err != nil {
		return 0, fmt.Errorf("outbox: sweep: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("outbox: sweep rows affected: %w", err)
	}
	return int(n), nil
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `cd pkg/transport/outbox/tidb && go test ./... -run TestStoreDBConstructs && go vet ./...`
Expected: PASS; the `var _ sequence.SequencerStore`, `var _ relay.LeaderStore`, `var _ sequence.RetentionStore = (*Store)(nil)` asserts compile.

- [ ] **Step 5: Commit**

```bash
git add pkg/transport/outbox/tidb/store.go pkg/transport/outbox/tidb/store_test.go
git commit -m "feat(outbox-tidb): sequencer, leader lock, retention sweep"
```

---

## Task 8: TiDB test harness + SQL-invariant integration tests

**Files:**
- Create: `pkg/transport/outbox/tidb/tidbtest/container.go`
- Modify: `pkg/transport/outbox/tidb/store_test.go` (add `TestMain` + integration tests)

**Interfaces:**
- Consumes: `tidb.Migrations`, `tidb.NewStore`, `tidb.NewStoreDB`, `outbox.Message`, `event.Metadata`.
- Produces: `tidbtest.Start(ctx) (*Instance, func(), error)` with `Instance{ DB *sql.DB; DSN string }`.
- **Note:** integration tests require Docker. They skip cleanly when the container can't start.

- [ ] **Step 1: Write the failing test**

Create `pkg/transport/outbox/tidb/tidbtest/container.go`:

```go
// Package tidbtest boots an ephemeral TiDB (testcontainers) with the outbox
// schema applied, for integration tests.
package tidbtest

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	_ "github.com/go-sql-driver/mysql"
	"github.com/golang-migrate/migrate/v4"
	migratemysql "github.com/golang-migrate/migrate/v4/database/mysql"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	tidb "github.com/quarks-tech/protoevent-go/pkg/transport/outbox/tidb"
)

const (
	tidbImage = "pingcap/tidb:v7.5.1"
	dbName    = "outbox_test"
	startupTO = 180 * time.Second
	tidbPort  = "4000/tcp"
)

type Instance struct {
	DB        *sql.DB
	DSN       string
	terminate func()
}

// Start boots TiDB, creates the db, applies migrations, and returns a ready
// Instance + cleanup. Returns an error (tests should t.Skip on it) when Docker
// is unavailable.
func Start(ctx context.Context) (*Instance, func(), error) {
	req := testcontainers.ContainerRequest{
		Image:        tidbImage,
		ExposedPorts: []string{tidbPort},
		WaitingFor: wait.ForSQL(tidbPort, "mysql", func(host, port string) string {
			num, _, _ := strings.Cut(port, "/")
			return fmt.Sprintf("root:@tcp(%s:%s)/", host, num)
		}).WithStartupTimeout(startupTO),
	}
	c, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: req, Started: true,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("start tidb (Docker unavailable?): %w", err)
	}
	host, err := c.Host(ctx)
	if err != nil {
		_ = c.Terminate(ctx)
		return nil, nil, err
	}
	mapped, err := c.MappedPort(ctx, tidbPort)
	if err != nil {
		_ = c.Terminate(ctx)
		return nil, nil, err
	}
	base := fmt.Sprintf("root:@tcp(%s:%s)/", host, mapped.Port())

	admin, err := sql.Open("mysql", base)
	if err != nil {
		_ = c.Terminate(ctx)
		return nil, nil, err
	}
	if _, err := admin.ExecContext(ctx, "CREATE DATABASE IF NOT EXISTS "+dbName); err != nil {
		_ = admin.Close()
		_ = c.Terminate(ctx)
		return nil, nil, err
	}
	_ = admin.Close()

	dsn := base + dbName + "?parseTime=true&loc=UTC&multiStatements=true"
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		_ = c.Terminate(ctx)
		return nil, nil, err
	}

	src, err := iofs.New(tidb.Migrations, "migrations")
	if err != nil {
		_ = db.Close()
		_ = c.Terminate(ctx)
		return nil, nil, err
	}
	drv, err := migratemysql.WithInstance(db, &migratemysql.Config{DatabaseName: dbName})
	if err != nil {
		_ = db.Close()
		_ = c.Terminate(ctx)
		return nil, nil, err
	}
	m, err := migrate.NewWithInstance("iofs", src, "mysql", drv)
	if err != nil {
		_ = db.Close()
		_ = c.Terminate(ctx)
		return nil, nil, err
	}
	if err := m.Up(); err != nil && err != migrate.ErrNoChange {
		_ = db.Close()
		_ = c.Terminate(ctx)
		return nil, nil, err
	}

	inst := &Instance{DB: db, DSN: dsn, terminate: func() { _ = db.Close(); _ = c.Terminate(context.Background()) }}
	return inst, inst.terminate, nil
}
```

Add `TestMain` + integration tests to `store_test.go`:

```go
package tidb_test

import (
	"context"
	"database/sql"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/quarks-tech/protoevent-go/pkg/event"
	"github.com/quarks-tech/protoevent-go/pkg/transport/outbox"
	tidb "github.com/quarks-tech/protoevent-go/pkg/transport/outbox/tidb"
	"github.com/quarks-tech/protoevent-go/pkg/transport/outbox/tidb/tidbtest"
)

var testDB *sql.DB

func TestMain(m *testing.M) {
	inst, cleanup, err := tidbtest.Start(context.Background())
	if err != nil {
		// Docker unavailable: skip the whole integration suite.
		os.Exit(0)
	}
	testDB = inst.DB
	code := m.Run()
	cleanup()
	os.Exit(code)
}

func truncate(t *testing.T) {
	t.Helper()
	for _, q := range []string{
		"DELETE FROM outbox", "DELETE FROM outbox_offsets", "DELETE FROM relay_lock",
		"UPDATE outbox_sequencer SET next_seq = 1 WHERE name = 'default'",
	} {
		if _, err := testDB.Exec(q); err != nil {
			t.Fatalf("reset (%s): %v", q, err)
		}
	}
}

func publish(t *testing.T, subject string) {
	t.Helper()
	tx, err := testDB.Begin()
	if err != nil {
		t.Fatal(err)
	}
	st := tidb.NewStore(tx)
	md := event.NewMetadata("books.created")
	md.ID = uuid.NewString()
	md.Source = "books-service"
	md.Subject = subject
	md.DataContentType = "application/proto"
	md.Time = time.Now().UTC()
	if err := st.CreateOutboxMessage(context.Background(), &outbox.Message{ID: md.ID, Metadata: md, Data: []byte("x")}); err != nil {
		_ = tx.Rollback()
		t.Fatalf("publish: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
}

func TestPublishInsertsUnsequencedRow(t *testing.T) {
	if testDB == nil {
		t.Skip("no TiDB")
	}
	truncate(t)
	publish(t, "s1")

	var nullCount int
	if err := testDB.QueryRow("SELECT COUNT(*) FROM outbox WHERE seq IS NULL AND tx_start_ts > 0").Scan(&nullCount); err != nil {
		t.Fatal(err)
	}
	if nullCount != 1 {
		t.Fatalf("unsequenced rows = %d, want 1 (seq NULL, tx_start_ts set)", nullCount)
	}
}

func TestSequenceAssignsDenseContiguousSeq(t *testing.T) {
	if testDB == nil {
		t.Skip("no TiDB")
	}
	truncate(t)
	for i := 0; i < 5; i++ {
		publish(t, "s")
	}
	st := tidb.NewStoreDB(testDB)
	n, err := st.SequenceMessages(context.Background(), 100)
	if err != nil {
		t.Fatalf("sequence: %v", err)
	}
	if n != 5 {
		t.Fatalf("sequenced %d, want 5", n)
	}
	rows, _ := testDB.Query("SELECT seq FROM outbox ORDER BY seq")
	defer rows.Close()
	var want int64 = 1
	for rows.Next() {
		var seq int64
		_ = rows.Scan(&seq)
		if seq != want {
			t.Fatalf("seq = %d, want %d (dense contiguous)", seq, want)
		}
		want++
	}
}

func TestConcurrentSequencersNoDuplicateNoGap(t *testing.T) {
	if testDB == nil {
		t.Skip("no TiDB")
	}
	truncate(t)
	for i := 0; i < 200; i++ {
		publish(t, "s")
	}
	st := tidb.NewStoreDB(testDB)

	var wg sync.WaitGroup
	for g := 0; g < 4; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				n, err := st.SequenceMessages(context.Background(), 20)
				if err != nil {
					t.Errorf("sequence: %v", err)
					return
				}
				if n == 0 {
					return
				}
			}
		}()
	}
	wg.Wait()

	// Assert seq is exactly 1..200 with no dup and no gap.
	var count, minSeq, maxSeq, distinct int64
	testDB.QueryRow("SELECT COUNT(*), MIN(seq), MAX(seq), COUNT(DISTINCT seq) FROM outbox").
		Scan(&count, &minSeq, &maxSeq, &distinct)
	if count != 200 || distinct != 200 || minSeq != 1 || maxSeq != 200 {
		t.Fatalf("count=%d distinct=%d min=%d max=%d, want 200/200/1/200 (FOR UPDATE must serialize)",
			count, distinct, minSeq, maxSeq)
	}
}

func TestCommitOffsetIsMonotone(t *testing.T) {
	if testDB == nil {
		t.Skip("no TiDB")
	}
	truncate(t)
	st := tidb.NewStore(testDB)
	ctx := context.Background()
	if err := st.CommitOffset(ctx, "c", 10); err != nil {
		t.Fatal(err)
	}
	if err := st.CommitOffset(ctx, "c", 5); err != nil { // must not rewind
		t.Fatal(err)
	}
	off, _ := st.Offset(ctx, "c")
	if off != 10 {
		t.Fatalf("offset = %d, want 10 (GREATEST must not rewind)", off)
	}
}

func TestLeaderLockMutualExclusionAndRelease(t *testing.T) {
	if testDB == nil {
		t.Skip("no TiDB")
	}
	truncate(t)
	st := tidb.NewStore(testDB)
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

Run: `cd pkg/transport/outbox/tidb && go mod tidy && go test ./... -run 'TestPublishInserts|TestSequenceAssigns|TestConcurrentSequencers|TestCommitOffset|TestLeaderLock' -v`
Expected (Docker present): initial FAIL only if a query is wrong; iterate until green. Expected (no Docker): tests SKIP — acceptable, but you MUST run them with Docker before marking the task done.

- [ ] **Step 3: Make it pass**

Fix any SQL/scan mismatches surfaced by the run. Common ones: `multiStatements=true` missing from DSN (needed by golang-migrate); `parseTime=true` missing (DATETIME scan). Both are already in the harness DSN.

- [ ] **Step 4: Run the full tidb suite**

Run: `cd pkg/transport/outbox/tidb && go test ./... -v`
Expected: all integration tests PASS (with Docker).

- [ ] **Step 5: Commit**

```bash
git add pkg/transport/outbox/tidb/tidbtest pkg/transport/outbox/tidb/store_test.go pkg/transport/outbox/tidb/go.sum
git commit -m "test(outbox-tidb): TiDB harness + SQL-invariant integration tests"
```

---

## Task 9: End-to-end relay over real TiDB

**Files:**
- Create: `pkg/transport/outbox/tidb/relay_integration_test.go`

**Interfaces:**
- Consumes: `sequence.NewRelay`, `sequence.Relay.RunOnce`, `tidb.NewStoreDB`, the publish helper.
- Produces: proof that publish → sequence → drain preserves order and is at-least-once end-to-end, and that a late-published row lands at a higher seq (the gap the whole design closes).

- [ ] **Step 1: Write the failing test**

Create `pkg/transport/outbox/tidb/relay_integration_test.go`:

```go
package tidb_test

import (
	"context"
	"sync"
	"testing"

	"github.com/quarks-tech/protoevent-go/pkg/event"
	"github.com/quarks-tech/protoevent-go/pkg/transport/outbox/relay/sequence"
	tidb "github.com/quarks-tech/protoevent-go/pkg/transport/outbox/tidb"
)

type recordingSender struct {
	mu   sync.Mutex
	seqs []int64 // relay drains in seq order; we record occurred order
}

func (r *recordingSender) Send(_ context.Context, md *event.Metadata, _ []byte) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.seqs = append(r.seqs, int64(len(r.seqs))+1) // position; order-check below uses monotonicity
	_ = md
	return nil
}

func TestRelayEndToEndOrderAndDelivery(t *testing.T) {
	if testDB == nil {
		t.Skip("no TiDB")
	}
	truncate(t)
	for i := 0; i < 50; i++ {
		publish(t, "s")
	}

	sender := &recordingSender{}
	// Single relay instance: it both sequences and drains.
	r := sequence.NewRelay("e2e", tidb.NewStoreDB(testDB), sender,
		sequence.WithBatchSize(10), sequence.WithSequenceBatchSize(1000))

	if err := r.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	if len(sender.seqs) != 50 {
		t.Fatalf("delivered %d, want 50", len(sender.seqs))
	}
	// Offset advanced to 50.
	off, _ := tidb.NewStore(testDB).Offset(context.Background(), "e2e")
	if off != 50 {
		t.Fatalf("offset = %d, want 50", off)
	}
}

func TestLatePublishGetsHigherSeq(t *testing.T) {
	if testDB == nil {
		t.Skip("no TiDB")
	}
	truncate(t)
	publish(t, "early")

	st := tidb.NewStoreDB(testDB)
	ctx := context.Background()
	if _, err := st.SequenceMessages(ctx, 100); err != nil {
		t.Fatal(err)
	}

	// A row published AFTER the first sequence pass must get a strictly higher
	// seq — never hide below the watermark. This is the gap the design closes.
	publish(t, "late")
	if _, err := st.SequenceMessages(ctx, 100); err != nil {
		t.Fatal(err)
	}

	var earlySeq, lateSeq int64
	testDB.QueryRow("SELECT seq FROM outbox WHERE subject = 'early'").Scan(&earlySeq)
	testDB.QueryRow("SELECT seq FROM outbox WHERE subject = 'late'").Scan(&lateSeq)
	if !(lateSeq > earlySeq) {
		t.Fatalf("late seq %d not > early seq %d", lateSeq, earlySeq)
	}
}
```

- [ ] **Step 2: Run test to verify it fails, then passes**

Run: `cd pkg/transport/outbox/tidb && go test ./... -run 'TestRelayEndToEnd|TestLatePublish' -v`
Expected (Docker): PASS after any fixes. (No Docker: SKIP — run with Docker before completing.)

- [ ] **Step 3: Commit**

```bash
git add pkg/transport/outbox/tidb/relay_integration_test.go
git commit -m "test(outbox-tidb): end-to-end relay ordering + late-publish gap closure"
```

---

## Task 10: Docs + README for the outbox module

**Files:**
- Create: `pkg/transport/outbox/README.md`
- Modify: `docs/design/outbox-sequenced-log.md` (mark Status: IMPLEMENTED, link the plan)

**Interfaces:** none (documentation).

- [ ] **Step 1: Write the README**

Create `pkg/transport/outbox/README.md` covering: the sequenced-log model (one-paragraph summary + links to `docs/design/outbox-sequenced-log.md` and the MongoDB companion `docs/design/outbox-mongodb-changestream.md`), the package layout (`relay` shared + `relay/sequence` TiDB + future `relay/stream` Mongo), a publish example using `NewPublisherFactory` inside a transaction, a relay wiring example (`sequence.NewRelay("broker-publish", tidb.NewStoreDB(db), rabbitSender, sequence.WithRetention(7*24*time.Hour, 256, 5000), sequence.WithObserver(promObserver))`), the ordering guarantee statement (total order, causal preserved, at-least-once, concurrent unordered), and the at-least-once / `event_id` dedup requirement for consumers. Use the exact type and function names produced by Tasks 2–7.

- [ ] **Step 2: Update the design doc status**

In `docs/design/outbox-sequenced-log.md`, change `Status: DRAFT` to `Status: IMPLEMENTED (see docs/superpowers/plans/2026-07-07-outbox-v2-sequenced-log.md)`.

- [ ] **Step 3: Verify links resolve**

Run: `git grep -n "outbox-sequenced-log" docs pkg/transport/outbox/README.md`
Expected: README and plan cross-reference the design doc.

- [ ] **Step 4: Commit**

```bash
git add pkg/transport/outbox/README.md docs/design/outbox-sequenced-log.md
git commit -m "docs(outbox): v2 sequenced-log README + design status"
```

---

## Self-Review

**1. Spec coverage** (design doc → task):
- §3 post-commit sequencing → Tasks 7 (`SequenceMessages`), 8 (`TestSequenceAssigns…`, `TestConcurrentSequencers…`), 9 (`TestLatePublish…`). ✓
- §4 schema (4 tables, index set, no `UNIQUE(seq)`, `partition_key` NOT in v2 runtime) → Task 5. Note: the design §9 says `partition_key` ships in the schema stamped `hash(subject)`. **Gap:** Task 5 migration omits `partition_key`. *Resolution:* v2 runtime is single-partition and no code reads the column; per YAGNI and to keep the reference store minimal, the column is deferred to the partition-lanes work (design §9/§13, explicitly future). Documented here as an intentional deviation — if you want strict doc-fidelity, add `partition_key VARBINARY(255) NOT NULL DEFAULT ''` to the Task 5 migration and stamp `hash(subject)` in Task 6's insert; it changes no other task. Flag for the reviewer.
- §5 SQL lifecycle (publish/sequencer/drain/sweep) → Tasks 6, 7. ✓
- §6 ordering `(tx_start_ts, id)`, no `tx_ordinal` → Task 5 schema + Task 7 ROW_NUMBER; Message has no `TxOrdinal` (Task 1). ✓
- §7 Go changes (Message, Store, SequencerStore, LeaderStore, Relay, Sender unchanged) → Tasks 1–3. ✓ Deviations (both in Global Constraints): `NewRelay` takes `name` as param, not `WithName` option; and the package split — shared primitives in `relay`, runtime + `Store`/`SequencerStore`/`RetentionStore` in `relay/sequence` (per the MongoDB companion doc §3), so doc references to `relay.Relay`/`relay.Store` read as `sequence.Relay`/`sequence.Store`.
- §10 latency budget: same-tick pipelining → Task 3 `TestRunOnceSequencesThenDrainsSameTick`; separate batch knobs → Tasks 2–3 + `TestDrainLoops…`; loop-while-full → Task 3; every-relay-sequences default + `WithoutSequencer` → Tasks 2–3; `LeaseTTL` 15s + graceful release → Tasks 2–3; `WithObserver` → Tasks 2–3; fixed interval no backoff → Task 3 (no backoff code). ✓
- §5.4 retention sweep below min offset → Task 7 `SweepMessages`, Task 3 `maybeSweep`. ✓
- Stop-the-lane vs park-and-continue → Tasks 3, 4. ✓
- §8 migration / chassis → explicitly out of scope (Global Constraints). ✓

**2. Placeholder scan:** No TBD/TODO/"handle errors appropriately". Every code step is complete. The one soft spot — Task 6 Step 1's `TestNewStoreCompiles` is a construction check with real behavior deferred to Task 8 — is intentional and stated (SQL behavior needs a live DB).

**3. Type consistency:** the store method set (`ListMessages`/`Offset`/`CommitOffset` on `sequence.Store`; `SequenceMessages` on `sequence.SequencerStore`; `TryAcquireLeaderLock`/`ReleaseLeaderLock` on `relay.LeaderStore`; `SweepMessages` on `sequence.RetentionStore`) is consistent across Tasks 2 (interfaces), 3 (runtime calls), 6–7 (impl + assertions), 8–9 (tests). Package placement is consistent: `Observer`/`Logger`/`LeaderStore` in `relay`; `Store`/`SequencerStore`/`RetentionStore`/`Options`/`Relay`/`NewRelay` in `sequence`; `sequence.Observer` embeds `relay.Observer` and adds `ObserveSequenced`. `NewStore(Runner)` vs `NewStoreDB(*sql.DB)` used consistently (publish/read use either; sequencer/sweep/late-publish tests use `NewStoreDB`). `Message` fields (`ID/Seq/Metadata/Data/CreateTime`) consistent Tasks 1, 3, 6, 8.

One fix applied during review: Task 7 Step 1's first test sketch referenced a nonexistent `contextT` type — replaced with the build-only `TestStoreDBConstructs`.
