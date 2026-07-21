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

    "github.com/quarks-tech/amqpx"
    "github.com/quarks-tech/protoevent-go/pkg/eventbus"
    "github.com/quarks-tech/protoevent-go/pkg/transport/rabbitmq"

    bookspb "yourapp/gen/books/v1"
)

func main() {
    ctx := context.Background()

    client := amqpx.NewClient(&amqpx.Config{
        Address: "guest:guest@localhost:5672/",
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
needed. Both give **at-least-once** delivery in commit order (equivalent to
one Kafka partition) and support independent consumer groups, each with its
own offset/resume token; a new group starts at "latest" (future events only)
by default.

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
        BookId: book.ID,
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
r, err := sequence.NewRelay("broker-publish", tidb.NewRelayStore(db), rabbitSender,
    sequence.WithRetention(7*24*time.Hour, 5*time.Minute, 5000),
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
st := mongodb.NewStore(db)
if err := st.EnsureIndexes(ctx); err != nil {
    log.Fatal(err)
}

r, err := stream.NewRelay("broker-publish", st, rabbitSender,
    stream.WithDrainWindow(time.Second),
    stream.WithLeaseTTL(15*time.Second),
)
if err != nil {
    log.Fatal(err)
}

if err := r.Run(ctx); err != nil {
    log.Fatal(err)
}
```

Both relays run leader election automatically when the store implements
`relay.LeaderStore` (both reference stores do), so multiple instances can run
for failover with only one processing at a time. Delivery is at-least-once,
not exactly-once: consumers **must** dedup on the event's CloudEvents
`Metadata.ID` (always the ID the publisher assigned; under the default
`ReuseMetadataID` it also keys the outbox row) for effectively-once
processing.

See [`pkg/transport/outbox/README.md`](pkg/transport/outbox/README.md) for
the full guide (package layout, ordering guarantees, design rationale,
benchmarks).

## License

MIT
