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
pkg/transport/outbox/relay/stream/     (future) MongoDB change-stream runtime — see the companion design doc
pkg/transport/outbox/tidb/       reference TiDB store implementation + schema migrations
```

`outbox` and `relay` have no database dependency; `tidb` is the reference storage
implementation (its own Go module, since it pulls in a SQL driver) and is a
template for other backends. A store only needs to implement the interfaces it
actually uses — e.g. a store that only ever runs behind `WithoutSequencer()` need
not implement `sequence.SequencerStore`.

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

```go
r := sequence.NewRelay("broker-publish", tidb.NewStoreDB(db), rabbitSender,
    sequence.WithRetention(7*24*time.Hour, 256, 5000),
    sequence.WithObserver(promObserver),
)

if err := r.Run(ctx); err != nil {
    log.Fatal(err)
}
```

`tidb.NewStoreDB(db)` (as opposed to `tidb.NewStore(r)` over a transaction-scoped
`Runner`) is required here because the sequencer, leader-lock, and retention sweep
each manage their own transactions against the pool. `Run` polls on
`sequence.WithPollInterval` (default 1s) until `ctx` is canceled, and on each tick:
acquires/renews the leader lock (`relay.LeaderStore`, if implemented) so only one
relay instance processes at a time, sequences newly committed rows, drains them to
`sender` in `seq` order, and — on the configured cadence — sweeps rows older than
the retention window and already consumed by every registered offset. Run multiple
`*Relay` instances (different processes) with the same name for automatic
failover; run relays with different names against the same store for independent
consumer groups with independent offsets.

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
