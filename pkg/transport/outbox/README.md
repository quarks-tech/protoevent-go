# outbox

Transactional outbox for protoevent-go: events are written to a durable log in the
same database transaction as the business change, then relayed to a downstream
transport (e.g. RabbitMQ) in commit order. Publish-time writes never block on the
broker, and a crashed relay simply resumes from the last committed offset — the
event is never silently dropped nor delivered before the transaction that produced
it has committed.

This package implements a **sequenced-log** design: the outbox table is an
append-only log, a leader-elected sequencer pass assigns a dense, gapless offset
(`seq`) to committed rows after the fact (so a transaction that started earlier but
committed later can never be skipped), and one or more relays drain the log in
`seq` order per consumer group. This README is the design reference: rationale and
guarantees live in the sections below, schema in the store modules' migrations and
godoc.

## Package layout

```
pkg/transport/outbox/            publish side (transport-agnostic): Message, Store, Sender, PublisherFactory
pkg/transport/outbox/relay/      primitives shared by every relay runtime: Observer, ErrorHandler, LeaderStore
pkg/transport/outbox/relay/sequence/   TiDB sequenced-log runtime: Store/SequencerStore/RetentionStore, Options, Relay
pkg/transport/outbox/relay/stream/     MongoDB change-stream runtime: Store/Stream/Event, Options, Relay
pkg/transport/outbox/tidb/       reference TiDB store implementation + schema migrations
pkg/transport/outbox/mongodb/    reference MongoDB store implementation (publish + stream.Store + LeaderStore)
```

`outbox` and `relay` have no database dependency; `tidb` and `mongodb` are the
reference storage implementations (each its own Go module, since they pull in a
driver) and are templates for other backends. A store only needs to implement
the interfaces it actually uses — e.g. a store that only ever runs behind
`WithoutSequencer()` need not implement `sequence.SequencerStore`, and a store
that only ever backs `relay/stream` need not implement `sequence.Store` at all.

## Publishing

Wrap a generated typed publisher (e.g. `bookspb.NewEventPublisher`) in a
`PublisherFactory`, then create a transaction-scoped publisher inside the same
database transaction as the business write:

```go
factory := outbox.NewPublisherFactory(bookspb.NewEventPublisher,
    outbox.WithPublisherOptions(
        eventbus.WithDefaultPublishOptions(
            eventbus.WithEventSource("books-service"),
        ),
    ),
)

err := txStore.WithTransaction(ctx, func(ctx context.Context, store MyStore) error {
    // MyStore embeds outbox.Store (CreateOutboxMessage) alongside your own
    // business-data methods, backed by the same *sql.Tx.
    if err := saveBook(ctx, store, book); err != nil {
        return err
    }

    return factory.Create(store).PublishBookCreatedEvent(ctx, &bookspb.BookCreatedEvent{
        BookId: book.ID,
    })
})
```

`factory.Create(store)` builds an `outbox.Sender` over `store` (via
`outbox.NewSender`) and wraps it in `eventbus.NewPublisher`, so publishing goes
through the normal `eventbus` interceptor chain and lands as a row in the outbox
table instead of going straight to the wire.

With the TiDB store the transaction is mandatory, not just good practice: the
row's `tx_start_ts` reads `@@tidb_current_ts`, which only exists on a
transactional connection — on an autocommit `*sql.DB` it reads 0, and
`CreateOutboxMessage` fails loudly rather than write a row that would sort
before every transactional row in a sequencer batch. (DSN tip: with
`go-sql-driver/mysql`, set `interpolateParams=true`, or every parameterized
query — including the publish INSERT inside your business transaction — pays a
prepare/exec/close cycle of three wire round trips.)

By default the outbox row ID is `outbox.ReuseMetadataID`: the row's ID is the
publisher's CloudEvents `Metadata.ID`, so the event the relay eventually emits
carries exactly the ID the publisher assigned, end to end. Because CloudEvents
IDs are UUIDs, this also scatters inserts across TiDB regions — hotspot
avoidance and identity preservation come for free together. Pass
`outbox.WithSenderOptions(outbox.WithIDGenerator(outbox.GenerateV4))` via
`FactoryOption` if you need the row's identity decoupled from the event's
`Metadata.ID` instead — the relayed event still carries the published
`Metadata.ID`, which travels in the persisted metadata document under either
generator. One fidelity caveat: stores persist metadata as JSON, so numeric
extension values round-trip as `float64` (encode exact numeric types as
strings).

## Schema (TiDB)

The `tidb` module embeds its schema as golang-migrate-compatible migrations
(`tidb.Migrations`, an `embed.FS` over `tidb/migrations/`). Apply them once
before publishing or relaying — the migration creates the `outbox_messages`,
`outbox_sequencers`, `outbox_offsets`, and `relay_locks` tables and seeds the
sequencer counter row:

```go
src, _ := iofs.New(tidb.Migrations, "migrations") // migrate/v4/source/iofs
drv, _ := mysql.WithInstance(db, &mysql.Config{}) // migrate/v4/database/mysql
m, _ := migrate.NewWithInstance("iofs", src, "mysql", drv)
if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
    log.Fatal(err)
}
```

## Relaying

`sequence.NewRelay` builds a relay for one named consumer group. `store` must
satisfy `sequence.Store` (read + offset); if it also implements
`sequence.SequencerStore`, the relay runs the post-commit sequencer pass itself
unless `sequence.WithoutSequencer()` is set. When several consumer groups share
one store, run the sequencer in exactly one relay and configure the others with
`WithoutSequencer()` — extra passes are harmless for correctness (they
serialize on the counter row) but waste serialized DB work every tick. `sender`
is the downstream transport, any `eventbus.Sender`. `NewRelay` returns an error
if `name` is empty (it keys the offset row and the default leader lock); if
`PollInterval`, `BatchSize`, `SequenceBatchSize`, or `LeaseTTL` is not strictly
positive; if `PollInterval` is not strictly less than `LeaseTTL/2` (the lease
must be renewable at least twice per TTL); and if `WithRetention` is given a
nonzero window without a strictly positive `sweepEvery` and `sweepBatch`. A
`Relay` is not safe for concurrent use: call `Run` from a single goroutine.

```go
r, err := sequence.NewRelay("broker-publish", tidb.NewRelayStore(db), rabbitSender,
    sequence.WithRetention(7*24*time.Hour, 256, 5000),
    sequence.WithObserver(promObserver),
)
if err != nil {
    log.Fatal(err)
}

if err := r.Run(ctx); err != nil {
    log.Fatal(err)
}
```

`tidb.NewRelayStore(db)` (as opposed to `tidb.NewStore(r)` over a transaction-scoped
`Runner`, which only implements the publish-side `outbox.Store`) is required here
because the sequencer, leader-lock, and retention sweep each manage their own
transactions against the pool. `Run` polls on
`sequence.WithPollInterval` (default 1s) until `ctx` is canceled, and on each tick:
acquires/renews the leader lock (`relay.LeaderStore`, if implemented) so only one
relay instance processes at a time, sequences newly committed rows, drains them to
`sender` in `seq` order, and — on the configured cadence — sweeps rows older than
the retention window and already consumed by every registered offset. Both the
sequencer and drain loops also renew the lease between full pages, so a pass over
a long backlog cannot outlive `LeaseTTL` — stale-leader overlap is bounded to a
single page. Run multiple `*Relay` instances (different processes) with the same
name for automatic failover; run relays with different names against the same
store for independent consumer groups with independent offsets. When a consumer
group is retired, decommission it with `DeleteOffset`: its stale offset row
otherwise keeps pinning `MIN(last_seq)` and halts the retention sweep forever.

**A new consumer group starts at "latest"** — its offset is seeded at the current
max seq, so it sees future events only (the same default as the MongoDB stream
runtime's start-at-now). Seeding is insert-if-absent (`InitOffsetLatest`): an
existing offset row — even one at 0 — is a committed position and is never
modified. Pass `sequence.WithStartFromBeginning()` to make a new group replay
the retained log instead; the option has no effect once the group has committed
an offset.

`sequence.WithObserver` wires an `Observer` (embeds `relay.Observer` plus
`ObserveSequenced`) to your metrics system — e.g. Prometheus — for lag and
throughput; `ObserveDrained`'s count is successfully sent messages only (parked
messages surface via `ObserveError`). `sequence.WithLogger` wires a
`*log/slog.Logger` for pass-level errors. `sequence.WithErrorHandler` switches
failure handling from stop-the-lane (default, order-preserving) to
park-and-continue: the `relay.ErrorHandler` is called for a message that failed
to send — or for a poison row whose persisted metadata fails to decode
(`sequence.DecodeError`) — and the relay advances past it; without a handler
the lane stops at the failed row. Shutdown cancellation is never routed to the
handler: a canceled run context stops the lane instead of parking healthy
messages.

## Ordering guarantee

Equivalent to one Kafka partition: total order per log (the sequencer assigns a
single dense counter), causal order preserved (a transaction that started after
another committed is never sequenced ahead of it), and **at-least-once** delivery.
Messages committed concurrently with no causal relationship may be sequenced and
delivered in either relative order.

## Consumer requirement: dedup on `event_id`

The relay guarantees at-least-once, not exactly-once: a redelivery can happen
after a drainer crashes between sending a page and committing its offset.
Consumers **must** dedup on the event's CloudEvents `Metadata.ID` — the relayed
event always carries the ID the publisher assigned, under any `IDGenerator`;
`event_id` is the outbox row's key, and the default `ReuseMetadataID` makes the
two coincide — to get effectively-once processing.

## MongoDB (change-stream relay)

`pkg/transport/outbox/relay/stream` is the second relay runtime, a sibling of
`relay/sequence` that reuses the same shared `relay` primitives (`Observer`,
`ErrorHandler`, `LeaderStore`). Instead of a leader-elected sequencer pass over a
polled table, it tails a MongoDB **change stream** on the insert-only `outbox_messages`
collection: MongoDB's oplog already gives a total, gapless commit order, so no
sequencer is needed. The lifecycle and resume-token cliff analysis are covered
in the sections below.

`pkg/transport/outbox/mongodb` is the reference `stream.Store` + publish
implementation over `*mongo.Database` (its own Go module, since it pulls in
`go.mongodb.org/mongo-driver/v2` — run `GOWORK=off go build ./...` /
`GOWORK=off go test ./...` inside it, since it is intentionally excluded from
this repo's `go.work`).

### Publishing

Publishing is unchanged from the TiDB path — the same `PublisherFactory`
wrapping a generated typed publisher, created inside a transaction — only the
transaction mechanism and store differ (a MongoDB session instead of a
`*sql.Tx`):

```go
factory := outbox.NewPublisherFactory(bookspb.NewEventPublisher,
    outbox.WithPublisherOptions(
        eventbus.WithDefaultPublishOptions(
            eventbus.WithEventSource("books-service"),
        ),
    ),
)

st := mongodb.NewStore(db) // db is a *mongo.Database

sess, err := db.Client().StartSession()
if err != nil {
    return err
}
defer sess.EndSession(ctx)

_, err = sess.WithTransaction(ctx, func(sc context.Context) (any, error) {
    if err := saveBook(sc, book); err != nil {
        return nil, err
    }

    return nil, factory.Create(st).PublishBookCreatedEvent(sc, &bookspb.BookCreatedEvent{
        BookId: book.ID,
    })
})
```

As with TiDB, the default `outbox.ReuseMetadataID` makes the row's `_id` the
publisher's CloudEvents `Metadata.ID`, so the event the relay eventually emits
carries exactly the ID the publisher assigned.

### Relaying

`stream.NewRelay` builds a relay for one named consumer group over a
`stream.Store` (`LoadToken`/`SaveToken`/`Watch`, all `string` resume
tokens — `string` rather than the driver's `bson.Raw` keeps the runtime
driver-free and the token immutable and comparable). It errors if `name` is empty, if `DrainWindow`,
`LeaseTTL`, or `TokenBatchSize` is not strictly positive, or if
`DrainWindow >= LeaseTTL/2`, since the leader lease must be renewable within a
single drain window. As with the sequence runtime, a `Relay` is not safe for
concurrent use: call `Run` from a single goroutine.

```go
st := mongodb.NewStore(db)
if err := st.EnsureIndexes(ctx); err != nil {
    log.Fatal(err)
}

r, err := stream.NewRelay("broker-publish", st, rabbitSender,
    stream.WithDrainWindow(time.Second),
    stream.WithLeaseTTL(15*time.Second),
    stream.WithObserver(promObserver),
)
if err != nil {
    log.Fatal(err)
}

if err := r.Run(ctx); err != nil {
    log.Fatal(err)
}
```

`EnsureIndexes` creates the TTL index on `outbox.create_time` (default 7 days,
tunable via `mongodb.WithRetention(d)`; idempotent for an unchanged retention —
safe to call on every startup). Changing retention on an existing collection
requires a `collMod` on the index, not a restart with a new option value;
`EnsureIndexes` surfaces that server error with a hint. There is no separate
`maxAwaitTime` knob: the relay passes `WithDrainWindow` to `Store.Watch` as
`maxAwait`, which becomes the change stream's `maxAwaitTimeMS` — one latency
knob, nothing to keep in sync. `stream.WithObserver`
wires the same `relay.Observer` shape used by `sequence.WithObserver` (e.g.
Prometheus) for lag and throughput (`ObserveDrained` counts successfully sent
messages only; parked messages surface via `ObserveError`);
`stream.WithLogger` wires a `*log/slog.Logger`
for stream-level errors; `stream.WithErrorHandler` switches send-failure
handling from stop-the-lane (default, order-preserving — closes and reopens
the cursor at the failed event) to park-and-continue (keeps the cursor open,
hands the failure to the callback). The same policy covers poison events whose
payload fails to decode (`stream.DecodeError`, which carries the event's resume
position): with a handler the relay parks the event and resumes past it;
without one the lane stops at it. Shutdown cancellation is never routed to the
handler — a canceled run context stops the lane instead of parking healthy
messages.

### Ordering, replay, and dedup

- **StartNow-only, no replay.** A consumer group with no stored token starts
  at "now" (no `resumeAfter`); there is currently no way to replay from the
  beginning of the outbox. This is a deliberate scope cut, not a limitation
  of the change stream itself.
- **Commit-order delivery**, per the single stream: causal order is preserved
  (a transaction that committed later is never delivered ahead of one that
  committed earlier), equivalent to one Kafka partition. As with the TiDB
  runtime, delivery is **at-least-once** — consumers **must** dedup on the
  event's CloudEvents `Metadata.ID` (always the ID the publisher assigned;
  under the default `ReuseMetadataID` it also keys the outbox row).
- **The resume-token cliff.** A relay that falls behind the oplog's retention
  window gets `ErrHistoryLost` (fatal — MongoDB's `ChangeStreamHistoryLost`)
  instead of resuming. This is handled with lag alerting on the committed
  token's age plus a break-glass runbook, not automatic replay.
  Operators **must** size the deployment so that **outbox TTL (default 7
  days, `mongodb.WithRetention`) > oplog window > consumer-downtime SLO**
  and alert on
  committed-token age well before it approaches the oplog window, so the
  cliff should never fire in practice.

## Benchmarks

Two tiers. Engine micro-benchmarks (`bench_test.go` in this package, plus
`relay/sequence` and `relay/stream`) run against in-memory fakes, need no
external services, and cover `Sender.Send`, a sequence-relay drain pass, and a
stream-relay drain window:

```bash
go test ./... -bench=. -run='^$'
```

Store-level benchmarks (`tidb/bench_test.go`, `mongodb/bench_test.go`) are
opt-in and containerized: they skip themselves unless Docker is available
(via `testcontainers`, the same `TestMain` the integration tests use), and
each store is its own Go module, so run with `GOWORK=off`:

```bash
GOWORK=off go test . -bench=. -run='^$'   # inside tidb/ or mongodb/
```

Numbers from these come from a container on the host running the benchmark,
not a production TiDB/MongoDB deployment — treat them as relative, not
absolute.
