# outbox

Transactional outbox for protoevent-go: events are written to a durable log in the
same database transaction as the business change, then relayed to a downstream
transport (e.g. RabbitMQ) in commit order. Publish-time writes never block on the
broker, and a crashed relay simply resumes from the last committed offset — the
event is never delivered before the transaction that produced it has committed,
and never silently dropped as long as retention outlives relay downtime —
each backend's sizing rule and recovery path is documented in its own section
(TiDB: the delivery-gated sweep window; MongoDB: retention > oplog window >
relay-downtime SLO, with the ErrHistoryLost runbook when that budget is
blown).

The relay runtimes implement a **sequenced-log** design over the
database-free publish API (the base `outbox` package only writes rows —
sequencing and ordered draining live in `relay/sequence` and `relay/stream`):
the outbox table is an append-only log, a leader-elected sequencer pass
assigns a dense, gapless offset (`seq`) to committed rows after the fact (so
a transaction that started earlier but committed later can never be skipped),
and one or more relays drain the log in `seq` order per consumer group. This README is the design reference: rationale and
guarantees live in the sections below, schema in the store modules' migrations and
godoc.

## Package layout

```text
pkg/transport/outbox/            publish side (transport-agnostic): Message, Store, Sender, PublisherFactory
pkg/transport/outbox/relay/      primitives shared by every relay runtime: Observer, PoisonHandler, LeaderStore
pkg/transport/outbox/relay/sequence/   TiDB sequenced-log runtime: Store/Sequencer/Sweeper, Options, Relay
pkg/transport/outbox/relay/stream/     MongoDB change-stream runtime: Store/Stream/Event, Options, Relay
pkg/transport/outbox/tidb/       reference TiDB store implementation + schema migrations
pkg/transport/outbox/tidb/tidbmigrate/  one-call schema migration (tidbmigrate.Apply); separate so publish-only builds skip migrate/v4
pkg/transport/outbox/mongodb/    reference MongoDB store implementation (publish + stream.Store + LeaderStore)
```

`outbox` and `relay` have no database dependency; `tidb` and `mongodb` are the
reference storage implementations (each its own Go module, since they pull in a
driver) and are templates for other backends. A store only needs to implement
the interfaces it actually uses — e.g. a store that only ever runs behind
`WithoutSequencer()` need not implement `sequence.Sequencer`, and a store
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
        Id: book.ID,
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
carries exactly the ID the publisher assigned, end to end. (The row's
*physical* primary key is a separate `AUTO_INCREMENT id` — a deliberate
schema trade-off documented in the migration header — so the UUID row ID
scatters only the `uk_outbox_event` unique-index writes, not the clustered
PK; the ID choice is about identity, not hotspot avoidance.) Pass
`outbox.WithSenderOptions(outbox.WithRowIDGenerator(outbox.GenerateUUIDv4))` via
`FactoryOption` if you need the row's identity decoupled from the event's
`Metadata.ID` instead — the relayed event still carries the published
`Metadata.ID`, which travels in the persisted metadata document under either
generator. One fidelity caveat: stores persist metadata as JSON, so numeric
extension values round-trip as `float64` (encode exact numeric types as
strings).

## Schema (TiDB)

Apply the schema once before publishing or relaying — it creates the
`outbox_messages`, `outbox_sequencers`, `outbox_offsets`, and `relay_locks`
tables and seeds the sequencer counter row. Use `tidbmigrate.Apply`, which
wraps the golang-migrate wiring in one call:

```go
import "github.com/quarks-tech/protoevent-go/pkg/transport/outbox/tidb/tidbmigrate"

if err := tidbmigrate.Apply(db); err != nil {
    log.Fatal(err)
}
```

DSN requirements for the migration connection: `multiStatements=true`, and
`tidb_skip_isolation_level_check=1` (golang-migrate runs its transaction at
SERIALIZABLE, which TiDB rejects without it), e.g.
`user:pass@tcp(host:4000)/db?parseTime=true&multiStatements=true&tidb_skip_isolation_level_check=1`.

`tidbmigrate` is its own package so publish-only builds importing `tidb`
never pull `migrate/v4` into their binaries. If you need raw control (custom
migrate tooling), the embedded FS is exported as `tidb.Migrations` /
`tidb.PrefixedMigrations(prefix)`.

### TiDB deployment checklist

1. **Migrate** — `tidbmigrate.Apply(db)` (prefixed instances:
   `tidbmigrate.WithTablePrefix`, which also gives each instance its own
   migrations-versions table). Skipping this fails loudly on first publish.
2. **DSN** — `parseTime=true` is REQUIRED for the relay (create_time scans
   into time.Time; without it every relay page fails with a
   `[]uint8 → time.Time` scan error while publishing keeps working) and
   `interpolateParams=true` is recommended (3 wire round trips → 1 per
   parameterized query).
3. **Publish** — `tidb.NewStore(tx)` inside the business transaction
   (autocommit fails loudly by design).
4. **Relay** — `sequence.NewRelay(name, tidb.NewRelayStore(db), sender, …)`;
   configure `WithRetention` — **without it the log grows forever** (there is
   deliberately no default sweep). Extra replicas of the same group need no
   extra config (leader election is automatic); extra consumer *groups* add
   `WithoutSequencer()` and should first start only after the sequencing
   relay is caught up (see its godoc's start-order caveat).
5. **Downstream sender** — a RabbitMQ sender needs its topology declared
   once: `rabbitmq.NewSender(client)` + `sender.Setup(ctx, &pb.EventbusServiceDesc)`
   (see the root README) — skipping `Setup` fails every relay tick on a
   fresh vhost.

### Multiple outbox instances in one schema

An outbox instance is the four-table set (`outbox_messages`, `outbox_offsets`,
`outbox_sequencers`, `relay_locks`). To run several independent instances in
one schema — each with its own total order, retention, and consumer groups
(the same lever as separate Kafka topics; a separate database is not an
alternative on TiDB, because the publish INSERT must share the business
transaction's schema) — give each instance a table prefix, on BOTH the
publish store and the relay store:

```go
st := tidb.NewStore(tx, tidb.WithTablePrefix("orders_"))
rs := tidb.NewRelayStore(db, tidb.WithTablePrefix("orders_"))
```

Migrate each instance with `tidbmigrate.WithTablePrefix`, which rewrites the
DDL AND gives each instance its own golang-migrate versions table (without a
separate versions table, instances silently skip each other's DDL):

```go
if err := tidbmigrate.Apply(db, tidbmigrate.WithTablePrefix("orders_")); err != nil {
    log.Fatal(err)
}
```

## Relaying

`sequence.NewRelay` builds a relay for one named consumer group. `store` must
satisfy `sequence.Store` (read + offset); if it also implements
`sequence.Sequencer`, the relay runs the post-commit sequencer pass itself
unless `sequence.WithoutSequencer()` is set. When several consumer groups share
one store, run the sequencer in exactly one relay and configure the others with
`WithoutSequencer()` — extra passes are harmless for correctness (they
serialize on the counter row) but waste serialized DB work every tick. `sender`
is the downstream transport, any `eventbus.Sender`. `NewRelay` returns an error
if `name` is empty (it keys the offset row and the default leader lock); if
`PollInterval`, `BatchSize`, `SequenceBatchSize`, or `LeaseTTL` is not strictly
positive; if `PollInterval` is not strictly less than `LeaseTTL/2` (the lease
must be renewable at least twice per TTL); if `name` (or an overridden
`LeaderLockName`) exceeds 64 bytes (the reference schema's `VARCHAR(64)` key
columns — a relaxed `sql_mode` would silently truncate a longer name into
another group's offset row and leader lock); if `WithRetention` is given a
nonzero window without a strictly positive `sweepInterval` and `sweepBatch`;
if the store lacks `sequence.Sequencer` without a `WithoutSequencer()`
waiver (a missing sequencer is a permanent silent stall, not a mode); and if
`WithRetention` is set on a store lacking `sequence.Sweeper` (a
configured sweep that silently never runs would grow the log unboundedly). A
`Relay` is not safe for concurrent use: call `Run` from a single goroutine.
(`Run` is the production loop; `RunOnce` is exposed for tests and custom
drivers that want to own the tick.)

```go
r, err := sequence.NewRelay("broker-publish", tidb.NewRelayStore(db), rabbitSender,
    sequence.WithRetention(7*24*time.Hour, 5*time.Minute, 5000),
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

`sequence.WithObserver` wires a `relay.Observer` — a struct of nil-able
callbacks (`OnDrained`, `OnError`, `OnSequenced`; set only what you need, the
zero value discards everything) — to your metrics system, e.g. Prometheus, for
lag and throughput; `OnDrained`'s count is successfully sent messages only
(parked messages surface via `OnError`). `sequence.WithLogger` wires a
`*log/slog.Logger` for pass-level errors. `sequence.WithPoisonHandler` installs
the poison-parking hook: a row whose persisted metadata fails to decode
(`sequence.DecodeError`) is handed to the `relay.PoisonHandler` and the relay
advances past it — retrying a poison row can never succeed. Send failures are
NEVER parked, handler or not: a send failure is downstream trouble (broker
down, timeout), and the lane always stops and retries the same message next
tick — order and delivery preserved, which is the point of an outbox. Without
a handler a poison row stops the lane too. Shutdown cancellation is never
routed to the handler: a canceled run context stops the lane instead of
parking healthy messages.

## Ordering guarantee

Equivalent to one Kafka partition: total order per log (the sequencer assigns a
single dense counter), causal order preserved (a transaction that started after
another committed is never sequenced ahead of it), and **at-least-once** delivery.
Messages committed concurrently with no causal relationship may be sequenced and
delivered in either relative order.

### Transport order vs event time

The relay delivers in **transport order** (`seq` — transaction-begin order),
never in event-time (`occur_time` / `Metadata.Time`) order, and this is a
design decision, not a gap. Event time is caller-controlled and backdatable
(`WithEventTime`), so an event-time ordering key can be undercut by a FUTURE
row: once anything stamped 12:00:05 is delivered, a late arrival stamped
12:00:01 forces the transport to either break its promised order or hold every
delivery back a grace window — and any finite window still loses to a
sufficiently late straggler. Event time is also neither unique nor monotone,
so it cannot serve as a dense, replayable committed offset. This is the same
split Kafka makes: brokers deliver strictly in offset order, and event-time
semantics (windowing, watermarks, lateness) live in the consumer, which is the
only party that knows its own lateness budget.

What the relay DOES promise about event time: `Metadata.Time` travels inside
every relayed event verbatim, so consumers that need event-time ordering apply
a windowed reorder with their own grace period. And in the default path (no
`WithEventTime`), `Metadata.Time` is stamped at publish inside the same
transaction whose `tx_start_ts` orders the log — so seq order and event-time
order already agree to within transaction duration; they diverge only when a
producer deliberately backdates, which is exactly the case no transport can
reorder correctly.

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
`PoisonHandler`, `LeaderStore`). Instead of a leader-elected sequencer pass over a
polled table, it tails a MongoDB **change stream** on the insert-only `outbox_messages`
collection: MongoDB's oplog already gives a total, gapless commit order, so no
sequencer is needed. The lifecycle and resume-token cliff analysis are covered
in the sections below.

`pkg/transport/outbox/mongodb` is the reference `stream.Store` + publish
implementation over `*mongo.Database` (its own Go module, so the
`go.mongodb.org/mongo-driver/v2` dependency stays out of the engine; it is
part of this repo's `go.work`, so plain `go build ./...` / `go test ./...`
inside it just work).

### Publishing

Publishing is unchanged from the TiDB path — the same `PublisherFactory`
wrapping a generated typed publisher, created inside a transaction. The store
owns the transaction runner (`Store.WithTransaction` delegates to the
driver's `Session.WithTransaction`, keeping its transient-error and commit
retry loops) and hands the callback a **session-bound** store — the
transaction is a value in your hands, like the TiDB store's tx-scoped
`Runner`, not something threaded through a magic ctx:

```go
factory := outbox.NewPublisherFactory(bookspb.NewEventPublisher,
    outbox.WithPublisherOptions(
        eventbus.WithDefaultPublishOptions(
            eventbus.WithEventSource("books-service"),
        ),
    ),
)

st := mongodb.NewStore(db) // db is a *mongo.Database

err := st.WithTransaction(ctx, func(ctx context.Context, tx *mongodb.Store) error {
    // ctx is the driver's session context, so your own collection writes
    // join the same transaction with no extra wiring.
    if err := saveBook(ctx, book); err != nil {
        return err
    }

    return factory.Create(tx).PublishBookCreatedEvent(ctx, &bookspb.BookCreatedEvent{
        Id: book.ID,
    })
})
```

If your repository layer owns the session (one transaction across several
stores — the `MyStore` embedding pattern from the TiDB example), bind
explicitly instead: run `sess.WithTransaction` yourself and use
`st.WithSession(sess)` — the bound store joins that session's transaction
even on a plain ctx, and fails loudly if called with a ctx carrying a
DIFFERENT session or with no transaction running. A raw session-ctx publish
on the unbound store keeps working (the guard accepts it), but the bound
forms make the transaction visible in the types.

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
`EnsureIndexes` surfaces that server error with a hint. Several independent
outbox instances can share one database via
`mongodb.WithCollectionPrefix("orders_")` — all three collections
(`outbox_messages`, `outbox_offsets`, `relay_locks`) get the prefix as a
unit, on both the publish and relay sides; a separate `*mongo.Database` per
instance works too (session transactions span databases within a cluster). There is no separate
`maxAwaitTime` knob: the relay passes `WithDrainWindow` to `Store.Watch` as
`maxAwait`, which becomes the change stream's `maxAwaitTimeMS` — one latency
knob, nothing to keep in sync. `stream.WithObserver`
wires the same `relay.Observer` callback struct used by `sequence.WithObserver`
(`OnSequenced` simply never fires here) for lag and throughput (`OnDrained`
counts successfully sent messages only; parked messages surface via `OnError`);
`stream.WithLogger` wires a `*log/slog.Logger`
for stream-level errors; `stream.WithPoisonHandler` installs the poison-parking
hook: an event whose payload fails to decode (`stream.DecodeError`, which
carries the event's resume position) is handed to the callback and the relay
resumes past it — retrying a poison event can never succeed. Send failures are
NEVER parked, handler or not: the lane always stops (closes the cursor,
persists the sent prefix, and reopens at the failed event after a backoff) —
order and delivery preserved. Without a handler a poison event stops the lane
too. Shutdown cancellation is never routed to the handler — a canceled run
context stops the lane instead of parking healthy messages.

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
  under the default `ReuseMetadataID` it also keys the outbox row). Delivery
  is transport order, never event-time order — the reasoning in
  [Transport order vs event time](#transport-order-vs-event-time) applies to
  this runtime verbatim (only the transport key differs: resume token /
  commit order instead of `seq`).
- **The resume-token cliff.** A relay that falls behind the oplog's retention
  window gets `ErrHistoryLost` (fatal — MongoDB's `ChangeStreamHistoryLost`)
  instead of resuming — surfaced both when a live stream's resume fails AND
  when a restarted relay's `Watch` re-open is rejected at aggregate time.
  This is handled with lag alerting on the committed
  token's age plus the break-glass runbook below, not automatic replay.
  Operators **must** size the deployment so that **outbox TTL (default 7
  days, `mongodb.WithRetention`) > oplog window > consumer-downtime SLO**
  and alert on
  committed-token age well before it approaches the oplog window, so the
  cliff should never fire in practice.

### MongoDB deployment checklist

1. **Topology** — both the publish path (multi-document transactions) and the
   relay (change streams) require a **replica set** (a single-node replica
   set is fine for dev; a standalone `mongod` fails both with raw server
   errors).
2. **`EnsureIndexes`** — call it on every startup (idempotent). Skipping it
   means **no TTL index is ever created and the outbox collection grows
   forever, silently** — `WithRetention` alone changes nothing.
3. **Publish** — inside `store.WithTransaction` (or on a
   `store.WithSession(sess)`-bound copy under your own session runner); an
   unbound, transactionless publish is rejected loudly.
4. **Relay** — `stream.NewRelay(name, store, sender, …)`; replicas of one
   group need no extra config (leader election is automatic). Size
   `TokenBatchSize × worst-case Send latency < LeaseTTL` (see NewRelay's
   godoc).
5. **Alerting** — export `OnDrained`'s committed-token age and alert well
   below the oplog window; ALSO alert on `OnDrained` going silent while
   `OnError` fires (see Observability below).

### Runbook: `ErrHistoryLost` (resume token off the oplog)

The relay exits `Run` with `ErrHistoryLost`. **A restart will not fix it** —
the stored token is permanently unusable, and each restart re-exits with the
same fatal (expect a crash loop until you intervene). Events are NOT lost:
they are still in the outbox collection (TTL permitting); what is lost is the
stream position.

1. **Stop the relay replicas** for the affected consumer group (halt the
   crash loop).
2. **Record the gap start**: the group's stored position timestamp —
   `db.outbox_offsets.findOne({_id: "<group>"}).cluster_time` (prefixed
   instances: `<prefix>outbox_offsets`).
3. **Reset the stored token** so the next start opens at "now":
   `store.DeleteToken(ctx, "<group>")` (or, in mongosh,
   `db.outbox_offsets.deleteOne({_id: "<group>"})`).
4. **Restart the relay and record the replay ceiling**: note the wall-clock
   time of the restart — `replayUntil := time.Now()`. The fresh group
   delivers everything from "now" onward live, so the replay must stop
   there: events after `replayUntil` are NOT part of the gap.
   Ordering is NOT preserved during break-glass recovery: the restarted
   relay delivers new events live while step 5 back-fills older ones. This
   is deliberate — restarting FIRST closes the loss window (replay-first
   would open a fresh gap between the replay ceiling and the restart), and
   the inversion is absorbed by the same consumer `Metadata.ID` idempotency
   that at-least-once already requires. A consumer that needs strict order
   must pause its own processing until step 5 completes.
5. **Re-send the gap**: read the outbox collection for the missed window and
   re-publish through the relay's own sender —

   Abort on the FIRST error — skipping past a failed decode or send during
   recovery silently drops that event forever (the token was already reset).
   Bound BOTH ends of the window: without the `$lt` ceiling the replay
   re-reads every event published after the restart — already delivered
   live — and on a busy outbox never catches up. The overlap margins on
   both ends are absorbed by consumer dedup; a missed event is not:

   ```go
   // Prefixed instances (WithCollectionPrefix): set collPrefix to match, so this
   // reads "<prefix>outbox_messages" alongside the "<prefix>outbox_offsets" row
   // consulted in step 2.
   const collPrefix = ""
   cur, err := db.Collection(collPrefix+"outbox_messages").Find(ctx,
       bson.M{"create_time": bson.M{
           // overlap must cover BOTH the worst-case oplog lag AND the clock
           // skew between the domains being compared: gapStart is server
           // clusterTime, create_time is publisher-stamped. e.g. 5m.
           "$gte": gapStart.Add(-overlap),
           "$lt":  replayUntil.Add(overlap),  // ceiling from step 4: live delivery owns everything after
       }},
       options.Find().SetSort(bson.D{{Key: "create_time", Value: 1}}))
   if err != nil {
       return err
   }
   defer cur.Close(ctx)
   for cur.Next(ctx) {
       var doc struct {
           Metadata []byte `bson:"metadata"`
           Data     []byte `bson:"data"`
       }
       if err := cur.Decode(&doc); err != nil {
           return err // fix the row, rerun the replay from gapStart
       }
       var md event.Metadata
       if err := json.Unmarshal(doc.Metadata, &md); err != nil {
           return err // poison row: park it manually, then rerun
       }
       if err := sender.Send(ctx, &md, doc.Data); err != nil {
           return err // downstream failed: rerun once it recovers (dedup absorbs)
       }
   }
   if err := cur.Err(); err != nil {
       return err
   }
   ```

   Order within the window is `create_time` (approximate), not exact commit
   order — and the overlap re-sends already-delivered events. Both are
   absorbed by the consumer `Metadata.ID` dedup contract.
6. **Post-mortem the sizing**: the cliff firing means downtime exceeded the
   oplog window — grow the oplog, shorten downtime, or tighten the
   token-age alert.

### Runbook: `ErrInvalidated` (outbox collection dropped/renamed)

Same shape: fatal, restart is a crash loop (the stored token references the
dead collection's identity).

1. Stop the relay replicas.
2. Recreate the collection: `EnsureIndexes` (any store construction path).
3. `store.DeleteToken(ctx, "<group>")` for EVERY consumer group of the
   instance.
4. Restart relays (each starts at "now").
5. If the drop lost undelivered events, that data is gone — restore from
   backup into the collection BEFORE step 4, or accept the gap. The
   `ErrHistoryLost` re-send procedure (step 5 above) works against a
   restored collection.

### Observability

`OnDrained` (and its `oldestAge`/committed-token age) fires only when a
drain pass RUNS. During a store outage the pass fails before draining — the
lag gauge **freezes at its last healthy value** while real lag grows. Alert
on both: (a) token/oldest age above threshold, AND (b) `OnDrained` absent
while `OnError` fires. To distinguish failure classes in `OnError`, classify
the error: `errors.Is(err, stream.ErrHistoryLost)` / `stream.ErrInvalidated`
(fatal — page someone), send failures carry your broker's error types
(downstream outage — the lane is stopped and retrying), anything else is
store trouble. `OnLeadership` marks takeovers, so handover timelines are
reconstructable; `OnSwept` at a constant full batch means retention is
falling behind (TiDB runtime).

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
(via `testcontainers`, the same `TestMain` the integration tests use):

```bash
go test . -bench=. -run='^$'   # inside tidb/ or mongodb/
```

Numbers from these come from a container on the host running the benchmark,
not a production TiDB/MongoDB deployment — treat them as relative, not
absolute.
