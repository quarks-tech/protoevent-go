# protoevent-go

A Go library for building event-driven applications using Protocol Buffers with CloudEvents-compatible metadata.

## Features

- Publish-subscribe event bus with CloudEvents 1.0 specification
- Multiple encoding formats (Proto, JSON)
- Pluggable transport mechanisms:
  - In-memory Go channels (`gochan`)
  - RabbitMQ (`pkg/transport/rabbitmq`)
  - Transactional Outbox (`pkg/transport/outbox`) — sequenced-log (TiDB) and
    change-stream (MongoDB) relay backends
- Code generation from proto definitions
- Interceptor chains for cross-cutting concerns
- Pluggable event ID generation (UUID v4 by default)

## Installation

```bash
go get github.com/quarks-tech/protoevent-go
```

For RabbitMQ transport:
```bash
go get github.com/quarks-tech/protoevent-go/pkg/transport/rabbitmq
```

For Transactional Outbox transport (the engine plus the store backend you use —
each is its own module):

```bash
go get github.com/quarks-tech/protoevent-go/pkg/transport/outbox
go get github.com/quarks-tech/protoevent-go/pkg/transport/outbox/tidb     # TiDB store
go get github.com/quarks-tech/protoevent-go/pkg/transport/outbox/mongodb  # MongoDB store
```

## Usage

### Define Events (Proto)

```protobuf
syntax = "proto3";

package example.books.v1;

import "quarks_tech/protoevent/v1/options.proto";

option go_package = "yourapp/gen/books/v1;bookspb";

message BookCreatedEvent {
  option (quarks_tech.protoevent.v1.enabled) = true;

  string id = 1;
  string title = 2;
  string author = 3;
}
```

### Generate Code

```bash
protoc --go-eventbus_out=. --go-eventbus_opt=paths=source_relative books.proto
```

### In-Memory Transport (gochan)

```go
package main

import (
    "context"
    "log"

    "github.com/quarks-tech/protoevent-go/pkg/eventbus"
    "github.com/quarks-tech/protoevent-go/pkg/transport/gochan"

    bookspb "yourapp/gen/books/v1"
)

func main() {
    ctx := context.Background()

    // Create transport
    transport := gochan.New()

    // Publisher
    publisher := eventbus.NewPublisher(transport,
        eventbus.WithDefaultPublishOptions(
            eventbus.WithEventSource("books-service"),
        ),
    )
    booksPublisher := bookspb.NewEventPublisher(publisher)

    // Subscriber
    subscriber := eventbus.NewSubscriber("books-consumer")
    bookspb.RegisterBookCreatedEventHandler(subscriber, &BookHandler{})

    // Start subscriber
    go func() {
        if err := subscriber.Subscribe(ctx, transport); err != nil {
            log.Fatal(err)
        }
    }()

    // Publish event
    err := booksPublisher.PublishBookCreatedEvent(ctx, &bookspb.BookCreatedEvent{
        Id:     "123",
        Title:  "The Go Programming Language",
        Author: "Alan Donovan",
    })
    if err != nil {
        log.Fatal(err)
    }
}

type BookHandler struct{}

func (h *BookHandler) Handle(ctx context.Context, event *bookspb.BookCreatedEvent) error {
    log.Printf("Book created: %s by %s", event.Title, event.Author)
    return nil
}
```

### Event ID Generation

Every published event gets an ID written to its CloudEvents metadata. By default
the publisher mints a random **UUID v4**. You can override how IDs are
generated, or supply an ID explicitly per publish:

```go
// Custom generator for all events from this publisher
publisher := eventbus.NewPublisher(transport,
    eventbus.WithIDGenerator(func() (string, error) {
        return ulid.Make().String(), nil
    }),
)

// Or supply an ID for a single event (skips the generator)
booksPublisher.PublishBookCreatedEvent(ctx, event,
    eventbus.WithEventID("my-explicit-id"),
)
```

A caller-supplied `WithEventID` always wins; the generator only runs when no ID
was provided.

### RabbitMQ Transport

```go
package main

import (
    "context"
    "log"

    "github.com/quarks-tech/amqpx"
    "github.com/quarks-tech/protoevent-go/pkg/eventbus"
    "github.com/quarks-tech/protoevent-go/pkg/transport/rabbitmq"

    bookspb "yourapp/gen/books/v1"
)

func main() {
    ctx := context.Background()

    // Create AMQP client
    client := amqpx.NewClient(&amqpx.Config{
        Address: "guest:guest@localhost:5672/",
    })
    defer func() {
        if err := client.Close(); err != nil {
            log.Printf("close AMQP client: %v", err)
        }
    }()

    // Publisher
    sender := rabbitmq.NewSender(client)
    if err := sender.Setup(ctx, &bookspb.EventbusServiceDesc); err != nil {
        log.Fatal(err)
    }

    publisher := eventbus.NewPublisher(sender,
        eventbus.WithDefaultPublishOptions(
            eventbus.WithEventSource("books-service"),
        ),
    )
    booksPublisher := bookspb.NewEventPublisher(publisher)

    // Publish event
    err := booksPublisher.PublishBookCreatedEvent(ctx, &bookspb.BookCreatedEvent{
        Id:     "123",
        Title:  "The Go Programming Language",
        Author: "Alan Donovan",
    })
    if err != nil {
        log.Fatal(err)
    }
}
```

#### RabbitMQ Subscriber

```go
package main

import (
    "context"
    "log"
    "os/signal"
    "syscall"

    "github.com/quarks-tech/amqpx"
    "github.com/quarks-tech/protoevent-go/pkg/eventbus"
    "github.com/quarks-tech/protoevent-go/pkg/transport/rabbitmq"

    bookspb "yourapp/gen/books/v1"
)

func main() {
    // SIGINT/SIGTERM cancels ctx; the receiver then drains in-flight
    // deliveries (amqpx ProcessWithDrain) and Subscribe returns nil on a
    // clean shutdown.
    ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
    defer stop()

    client := amqpx.NewClient(&amqpx.Config{
        Address: "guest:guest@localhost:5672/",
        // REQUIRED if this process also PUBLISHES through the same client. Each
        // running subscription holds one pool connection for as long as it runs,
        // and PoolSize defaults to GOMAXPROCS — which is cgroup-aware, so a
        // 1-CPU pod gets a pool of ONE and every publish then fails with
        // "connection pool timeout", permanently. Size it to
        // subscriptions + 1 (or give the publisher its own client).
        PoolSize: 2,
        // Consumer drain budget on shutdown (amqpx ProcessWithDrain): in-flight
        // deliveries get this long to finish before the connection is
        // force-closed. Default 30s — size it to the deployment's termination
        // grace period.
        // DrainTimeout: 20 * time.Second,
    })
    defer func() {
        if err := client.Close(); err != nil {
            log.Printf("close AMQP client: %v", err)
        }
    }()

    // Subscriber with topology setup
    subscriber := eventbus.NewSubscriber("books-consumer")
    bookspb.RegisterBookCreatedEventHandler(subscriber, &BookHandler{})

    receiver := rabbitmq.NewReceiver(client,
        rabbitmq.WithTopologySetup(),
        rabbitmq.WithDLX(),
        rabbitmq.WithPrefetchCount(10),
    )

    if err := subscriber.Subscribe(ctx, receiver); err != nil {
        log.Fatal(err)
    }
}
```

#### RabbitMQ with Parking Lot (Dead Letter + Retry)

```go
import "github.com/quarks-tech/protoevent-go/pkg/transport/rabbitmq/parkinglot"

receiver := parkinglot.NewReceiver(client,
    parkinglot.WithTopologySetup(),
    parkinglot.WithBindingsSetup(),
    parkinglot.WithMaxRetries(3),
    parkinglot.WithMinRetryBackoff(15 * time.Second),
)
```

#### Receiver defaults: what a failing handler costs the queue

A handler that returns a plain error gets its delivery **requeued** (not
discarded), and the requeue is **paced** — the delay starts at 200ms, doubles
with the quorum queue's `x-delivery-count`, and caps at 5s. Both halves matter:
an unpaced requeue loop burns a quorum queue's whole `x-delivery-limit` at broker
speed (measured: 21 attempts in 13.9ms) and the broker then discards the event,
while pacing turns that budget into a window of about 76s for the fault to clear.

The cost is that **the delay stops the whole consumer, not just that delivery.**
Deliveries are processed one at a time on a single goroutine — prefetch buys
buffering, never concurrency — so while one message is being paced, the consumer
clears `prefetch - 1` others per delay. The defaults are chosen as a pair for
that reason:

| Option | Default | Why this value |
| --- | --- | --- |
| `WithPrefetchCount` | 16 | Sets how many messages get through per pacing delay (`prefetch - 1`). At the old default of 3 a single permanently-failing message dropped a 4264 msg/s consumer to 0.13 msg/s for the message's whole delivery budget. |
| `WithRequeueBackoff` | 200ms → 5s | The cap is what everything behind the failing message pays. 5s keeps the 20-delivery retry budget at ~76s while tripling throughput during an episode. |

Retune them together — raising the cap or lowering the prefetch reverses the
trade. If a persistently-failing message must not stall the queue **at all**,
this receiver is the wrong shape: use `parkinglot.Receiver` above, where the wait
is served by a broker-side queue TTL instead of by the consume goroutine. A
permanently unprocessable event is a separate case and is never paced — return
`eventbus.NewUnprocessableEventError(err)` and it is rejected immediately
(dead-lettered under `WithDLX`).

#### High-throughput publishing (`SendBatch`)

`rabbitmq.Sender` implements the optional `eventbus.BatchSender` capability:
publish a run of events on one channel and wait for their confirms **together**,
instead of paying a full publish-and-confirm round trip per event.

```go
sent, err := sender.SendBatch(ctx, []eventbus.Outgoing{
    {Metadata: md1, Data: data1},
    {Metadata: md2, Data: data2},
})
// sent is the CONTIGUOUS confirmed prefix; err describes msgs[sent] specifically.
// err == nil if and only if sent == len(msgs).
```

The confirm is ~99% of a send (measured: 86,139 events/s with confirms off,
1,155/s with them on), so anything publishing serially is bound by one broker
round trip per event. Overlapping the waits replaces that with one round trip per
batch. **No guarantee is weakened** — every message is still individually
confirmed by the broker; only the waiting overlaps.

Most publishers never call this directly. The outbox relay discovers it by type
assertion and uses it automatically, which is where the effect shows: a backlog
drain went from 918 to 15,791 events/s, and from 130 to 5,424 events/s at the 5ms
confirm latency an ordinary cross-AZ quorum queue answers in. It falls back to
serial sending under `WithMandatoryPublish` (attributing a `basic.return` needs
exactly one publish in flight per channel) and `WithoutPublisherConfirms`
(nothing to overlap).

### Transactional Outbox Transport

The outbox transport implements the transactional outbox pattern: events are
written to a durable, append-only log in the same database transaction as the
business change, then relayed to a downstream transport (e.g. RabbitMQ) in
commit order, so publish-time writes never block on the broker. Two relay
runtimes share one publish-side API. On **TiDB**, a leader-elected sequencer
pass assigns a dense, gapless offset (`seq`) to committed rows after the
fact, so a transaction that started earlier but committed later is never
skipped. On **MongoDB**, a relay tails a change stream on the insert-only
outbox collection, reusing the oplog's total commit order — no sequencer
needed. Both give **at-least-once** delivery in a single total order
(equivalent to one Kafka partition) and support independent consumer groups,
each with its own offset/resume token; a new group starts at "latest" (future
events only) by default.

The two backends order by different things, and it matters for
read-modify-write consumers: MongoDB delivers in **commit** order (the oplog's),
while TiDB's `seq` is **transaction-begin** order (`tx_start_ts`). Two TiDB
transactions with overlapping lifetimes can therefore be delivered in the
reverse of the order they committed — even when they conflict on the same row.
No event is skipped or duplicated by this, but a consumer that applies
last-write-wins per aggregate must carry its own version/revision in the payload
rather than trusting delivery order. See "Not guaranteed" in
`docs/design/outbox-sequenced-log.md`.

```text
┌──────────────┐  insert (seq=NULL)   ┌──────────────────┐
│  business tx │ ────────────────────►│   outbox (log)    │
└──────────────┘                      └─────────┬────────┘
                                                  │ sequence (TiDB) /
                                                  │ change-stream tail (Mongo)
                                                  ▼
                                       ┌──────────────────┐
                                       │ relay (per group) │──► broker (e.g. RabbitMQ)
                                       │ offset/resume-tok │
                                       └──────────────────┘
```

#### Publish (both backends)

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
    // business-data methods, backed by the same *sql.Tx (or Mongo session).
    if err := saveBook(ctx, store, book); err != nil {
        return err
    }

    return factory.Create(store).PublishBookCreatedEvent(ctx, &bookspb.BookCreatedEvent{
        Id: book.ID,
    })
})
```

By default the outbox row ID is `outbox.ReuseMetadataID`: the row's ID is the
publisher's CloudEvents `Metadata.ID`, so the relayed event carries exactly
the ID the publisher assigned. Pass
`outbox.WithSenderOptions(outbox.WithRowIDGenerator(outbox.GenerateUUIDv4))` via
`FactoryOption` to decouple the row's identity from the event's instead.

#### Relay: TiDB (sequenced log)

```go
// The retention window is the store's (it applies store-wide); the relay owns
// only how often the sweep runs.
store := tidb.NewRelayStore(db, tidb.WithRetentionWindow(7*24*time.Hour))

r, err := sequence.NewRelay("broker-publish", store, rabbitSender,
    sequence.WithRetention(5*time.Minute, 5000),
)
if err != nil {
    log.Fatal(err)
}

if err := r.Run(ctx); err != nil {
    log.Fatal(err)
}
```

#### Relay: MongoDB (change-stream tail)

```go
// NewRelayStore, not NewStore: publishing is session-bound (the row commits with
// the caller's business writes), while relay operations manage their own
// transactions against the pool. Separate types keep the two from mixing.
rs := mongodb.NewRelayStore(db)
if err := rs.EnsureIndexes(ctx); err != nil {
    log.Fatal(err)
}

// DrainWindow is the one latency knob (it is also the change stream's
// server-side maxAwaitTime). LeaseTTL is left at its 60s default: the
// constructor requires DrainWindow + OpTimeout < LeaseTTL, so lowering the
// lease means lowering WithOpTimeout (default 30s) with it.
r, err := stream.NewRelay("broker-publish", rs, rabbitSender,
    stream.WithDrainWindow(time.Second),
)
if err != nil {
    log.Fatal(err)
}

if err := r.Run(ctx); err != nil {
    log.Fatal(err)
}
```

Both relays run leader election automatically over a store that implements
`relay.LeaderStore` (both reference stores do), so multiple instances can run
for failover with only one processing at a time. A store that cannot elect is a
construction error unless you pass `WithoutLeaderElection()`: an unelected relay
running on several replicas delivers the whole log from every one of them, so
single-instance mode has to be stated rather than inferred. Delivery is
at-least-once,
not exactly-once: consumers **must** dedup on the event's CloudEvents
`Metadata.ID` (always the ID the publisher assigned; under the default
`ReuseMetadataID` it also keys the outbox row) for effectively-once
processing.

See [`pkg/transport/outbox/README.md`](pkg/transport/outbox/README.md) for
the full guide (package layout, ordering guarantees, design rationale,
benchmarks).

## License

MIT
