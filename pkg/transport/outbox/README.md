# outbox

Transactional outbox for protoevent-go: events are written to a durable log in the
same database transaction as the business change, then relayed to a downstream
transport (e.g. RabbitMQ) in commit order. Publish-time writes never block on the
broker, and a crashed relay simply resumes from the last committed offset — the
event is never silently dropped nor delivered before the transaction that produced
it has committed.

This package implements the **v2 sequenced-log** design: the outbox table is an
append-only log, a leader-elected sequencer pass assigns a dense, gapless offset
(`seq`) to committed rows after the fact (so a transaction that started earlier but
committed later can never be skipped), and one or more relays drain the log in
`seq` order per consumer group. See the full design docs for the rationale, schema,
and race analysis:

- [`docs/design/outbox-sequenced-log.md`](../../../docs/design/outbox-sequenced-log.md) — TiDB sequenced-log relay (this package's reference runtime).
- [`docs/design/outbox-mongodb-changestream.md`](../../../docs/design/outbox-mongodb-changestream.md) — MongoDB companion design (change-stream tail, no sequencer needed).

## Package layout

```
pkg/transport/outbox/            publish side (transport-agnostic): Message, Store, Sender, PublisherFactory
pkg/transport/outbox/relay/      primitives shared by every relay runtime: Observer, Logger, LeaderStore
pkg/transport/outbox/relay/sequence/   TiDB sequenced-log runtime: Store/SequencerStore/RetentionStore, Options, Relay
pkg/transport/outbox/relay/stream/     MongoDB change-stream runtime: StreamStore/Stream/Event, Options, Relay
pkg/transport/outbox/tidb/       reference TiDB store implementation + schema migrations
pkg/transport/outbox/mongodb/    reference MongoDB store implementation (publish + StreamStore + LeaderStore)
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

By default the outbox row ID is `outbox.ReuseMetadataID`: the row's ID is the
publisher's CloudEvents `Metadata.ID`, so the event the relay eventually emits
carries exactly the ID the publisher assigned, end to end. Because CloudEvents
IDs are UUIDs, this also scatters inserts across TiDB regions — hotspot
avoidance and identity preservation come for free together. Pass
`outbox.WithSenderOptions(outbox.WithIDGenerator(outbox.GenerateV4))` via
`FactoryOption` if you need the row's identity decoupled from the event's
`Metadata.ID` instead.

## Relaying

`sequence.NewRelay` builds a relay for one named consumer group. `store` must
satisfy `sequence.Store` (read + offset); if it also implements
`sequence.SequencerStore`, the relay runs the post-commit sequencer pass itself
unless `sequence.WithoutSequencer()` is set (only one relay per outbox table
should sequence). `sender` is the downstream transport, any `eventbus.Sender`.
`NewRelay` returns an error if `PollInterval`, `BatchSize`,
`SequenceBatchSize`, or `LeaseTTL` is not strictly positive.

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
the retention window and already consumed by every registered offset. Run multiple
`*Relay` instances (different processes) with the same name for automatic
failover; run relays with different names against the same store for independent
consumer groups with independent offsets.

**A new consumer group starts at "latest"** — its offset is seeded at the current
max seq, so it sees future events only (the same default as the MongoDB stream
runtime's start-at-now). Pass `sequence.WithStartFromBeginning()` to make a new
group replay the retained log instead; the option has no effect once the group
has committed an offset.

`sequence.WithObserver` wires an `Observer` (embeds `relay.Observer` plus
`ObserveSequenced`) to your metrics system — e.g. Prometheus — for lag and
throughput; `sequence.WithLogger` wires a `relay.Logger` for pass-level errors.

## Ordering guarantee

Equivalent to one Kafka partition: total order per log (the sequencer assigns a
single dense counter), causal order preserved (a transaction that started after
another committed is never sequenced ahead of it), and **at-least-once** delivery.
Messages committed concurrently with no causal relationship may be sequenced and
delivered in either relative order.

## Consumer requirement: dedup on `event_id`

The relay guarantees at-least-once, not exactly-once: a redelivery can happen
after a drainer crashes between sending a page and committing its offset.
Consumers **must** dedup on the event's ID (the outbox row's `event_id`, which
under the default `ReuseMetadataID` generator equals the published CloudEvents
`Metadata.ID`) to get effectively-once processing.

## MongoDB (change-stream relay)

`pkg/transport/outbox/relay/stream` is the second relay runtime, a sibling of
`relay/sequence` that reuses the same shared `relay` primitives (`Observer`,
`Logger`, `LeaderStore`). Instead of a leader-elected sequencer pass over a
polled table, it tails a MongoDB **change stream** on the insert-only `outbox`
collection: MongoDB's oplog already gives a total, gapless commit order, so no
sequencer is needed. See
[`docs/design/outbox-mongodb-changestream.md`](../../../docs/design/outbox-mongodb-changestream.md)
for the full design — schema, lifecycle, and the resume-token cliff analysis.

`pkg/transport/outbox/mongodb` is the reference `StreamStore` + publish
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
`stream.StreamStore` (`LoadToken`/`SaveToken`/`Watch`, all `string` resume
tokens — see the design doc §8.2 for why the boundary is `string` rather than
the driver's `bson.Raw`). It errors if `DrainWindow >= LeaseTTL/2`, since the
leader lease must be renewable within a single drain window.

```go
st := mongodb.NewStore(db, mongodb.WithMaxAwaitTime(time.Second))
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

`EnsureIndexes` creates the 7-day TTL index on `outbox.create_time` (idempotent
— safe to call on every startup); `mongodb.WithMaxAwaitTime` (passed to
`NewStore`) sets the change stream's `maxAwaitTimeMS`, which should match
`WithDrainWindow`. `stream.WithObserver`
wires the same `relay.Observer` shape used by `sequence.WithObserver` (e.g.
Prometheus) for lag and throughput; `stream.WithLogger` wires a `relay.Logger`
for stream-level errors; `stream.WithErrorHandler` switches send-failure
handling from stop-the-lane (default, order-preserving — closes and reopens
the cursor at the failed event) to park-and-continue (keeps the cursor open,
hands the failure to the callback).

### Ordering, replay, and dedup (v1)

- **StartNow-only, no replay.** A consumer group with no stored token starts
  at "now" (no `resumeAfter`); there is no v1 way to replay from the
  beginning of the outbox. This is a deliberate v1 scope cut (design §7/§11),
  not a limitation of the change stream itself.
- **Commit-order delivery**, per the single stream: causal order is preserved
  (a transaction that committed later is never delivered ahead of one that
  committed earlier), equivalent to one Kafka partition. As with the TiDB
  runtime, delivery is **at-least-once** — consumers **must** dedup on
  `event_id`, which under the default `ReuseMetadataID` equals the published
  CloudEvents `Metadata.ID`.
- **The resume-token cliff.** A relay that falls behind the oplog's retention
  window gets `ErrHistoryLost` (fatal — MongoDB's `ChangeStreamHistoryLost`)
  instead of resuming. v1 handles this with lag alerting on the committed
  token's age plus a break-glass runbook (design §7), not automatic replay.
  Operators **must** size the deployment so that **oplog window > outbox TTL
  (7 days) > consumer-downtime SLO** (design §7) and alert on committed-token
  age well before it approaches the oplog window, so the cliff should never
  fire in practice.
