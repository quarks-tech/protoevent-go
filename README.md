# protoevent-go

A Go library for building event-driven applications using Protocol Buffers with CloudEvents-compatible metadata.

## Features

- Publish-subscribe event bus with CloudEvents 1.0 specification
- Multiple encoding formats (Proto, JSON)
- Pluggable transport mechanisms:
  - In-memory Go channels (`gochan`)
  - RabbitMQ (`pkg/transport/rabbitmq`)
  - Transactional Outbox (`pkg/transport/outbox`)
- Code generation from proto definitions
- Interceptor chains for cross-cutting concerns

## Installation

```bash
go get github.com/quarks-tech/protoevent-go
```

For RabbitMQ transport:
```bash
go get github.com/quarks-tech/protoevent-go/pkg/transport/rabbitmq
```

For Transactional Outbox transport:
```bash
go get github.com/quarks-tech/protoevent-go/pkg/transport/outbox
```

## Usage

### Define Events (Proto)

```protobuf
syntax = "proto3";

package example.books.v1;

import "quarks_tech/protoevent/v1/options.proto";

option go_package = "example/gen/example/books/v1;bookspb";

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

    bookspb "example/gen/example/books/v1"
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

### RabbitMQ Transport

```go
package main

import (
    "context"
    "log"

    "github.com/quarks-tech/amqpx"
    "github.com/quarks-tech/protoevent-go/pkg/eventbus"
    "github.com/quarks-tech/protoevent-go/pkg/transport/rabbitmq"

    bookspb "example/gen/example/books/v1"
)

func main() {
    ctx := context.Background()

    // Create AMQP client
    client := amqpx.NewClient(&amqpx.Config{
        Host:     "localhost",
        Port:     5672,
        Username: "guest",
        Password: "guest",
    })
    defer client.Close()

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

    bookspb "example/gen/example/books/v1"
)

func main() {
    ctx := context.Background()

    client := amqpx.NewClient(&amqpx.Config{
        Host:     "localhost",
        Port:     5672,
        Username: "guest",
        Password: "guest",
    })
    defer client.Close()

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

The outbox transport implements the transactional outbox pattern for reliable event publishing with database transactions.

#### Implement Store Interface (TiDB)

```go
package storage

import (
    "context"
    "database/sql"
    "encoding/json"
    "fmt"
    "strings"
    "time"

    "github.com/quarks-tech/protoevent-go/pkg/event"
    "github.com/quarks-tech/protoevent-go/pkg/transport/outbox"
)

// Store implements outbox.Store for transactional operations
type Store struct {
    db *sql.DB
}

func (s *Store) CreateMessage(ctx context.Context, msg *outbox.Message) error {
    metadata, err := json.Marshal(msg.Metadata)
    if err != nil {
        return fmt.Errorf("marshal metadata: %w", err)
    }

    _, err = s.db.ExecContext(ctx, `
        INSERT INTO outbox (id, metadata, data, create_time)
        VALUES (?, ?, ?, ?)
    `, msg.ID, metadata, msg.Data, msg.CreateTime)
    return err
}

// RelayStore implements outbox.RelayStore for relay operations
type RelayStore struct {
    db *sql.DB
}

func (s *RelayStore) GetOutboxCursor(ctx context.Context) (string, error) {
    var cursor sql.NullString
    err := s.db.QueryRowContext(ctx, `
        SELECT id FROM outbox WHERE sent_time IS NULL ORDER BY id LIMIT 1
    `).Scan(&cursor)
    if err == sql.ErrNoRows {
        return "", nil
    }
    return cursor.String, err
}

func (s *RelayStore) ListPendingOutboxMessages(ctx context.Context, cursor string, limit int) ([]*outbox.Message, error) {
    query := `
        SELECT id, metadata, data, create_time FROM outbox
        WHERE sent_time IS NULL AND id >= ?
        ORDER BY id LIMIT ?
    `
    rows, err := s.db.QueryContext(ctx, query, cursor, limit)
    if err != nil {
        return nil, err
    }
    defer rows.Close()

    var messages []*outbox.Message
    for rows.Next() {
        var (
            id           string
            metadataJSON []byte
            data         []byte
            createTime   time.Time
        )
        if err := rows.Scan(&id, &metadataJSON, &data, &createTime); err != nil {
            return nil, err
        }

        var metadata event.Metadata
        if err := json.Unmarshal(metadataJSON, &metadata); err != nil {
            return nil, fmt.Errorf("unmarshal metadata: %w", err)
        }

        messages = append(messages, &outbox.Message{
            ID:         id,
            Metadata:   &metadata,
            Data:       data,
            CreateTime: createTime,
        })
    }
    return messages, rows.Err()
}

func (s *RelayStore) UpdateOutboxMessagesSentTime(ctx context.Context, t time.Time, ids ...string) error {
    if len(ids) == 0 {
        return nil
    }

    placeholders := make([]string, len(ids))
    args := make([]any, 0, len(ids)+1)
    args = append(args, t)
    for i, id := range ids {
        placeholders[i] = "?"
        args = append(args, id)
    }

    query := fmt.Sprintf(`UPDATE outbox SET sent_time = ? WHERE id IN (%s)`, strings.Join(placeholders, ","))
    _, err := s.db.ExecContext(ctx, query, args...)
    return err
}

func (s *RelayStore) DeleteOutboxMessages(ctx context.Context, ids ...string) error {
    if len(ids) == 0 {
        return nil
    }

    placeholders := make([]string, len(ids))
    args := make([]any, 0, len(ids))
    for i, id := range ids {
        placeholders[i] = "?"
        args = append(args, id)
    }

    query := fmt.Sprintf(`DELETE FROM outbox WHERE id IN (%s)`, strings.Join(placeholders, ","))
    _, err := s.db.ExecContext(ctx, query, args...)
    return err
}
```

#### Publisher with Transactional Outbox

```go
package main

import (
    "context"

    "github.com/quarks-tech/protoevent-go/pkg/eventbus"
    "github.com/quarks-tech/protoevent-go/pkg/transport/outbox"

    bookspb "example/gen/example/books/v1"
)

type TxStore interface {
    Store
    WithTransaction(ctx context.Context, fn func(ctx context.Context, s Store) error) error
}

type Store interface {
    outbox.Store
    CreateBook(ctx context.Context, book *Book) error
}

func main() {
    // Create publisher factory
    publisherFactory := outbox.NewPublisherFactory(
        eventbus.WithDefaultPublishOptions(
            eventbus.WithEventSource("books-service"),
        ),
    )

    // Use within transaction
    err := txStore.WithTransaction(ctx, func(ctx context.Context, store Store) error {
        // Business logic
        if err := store.CreateBook(ctx, book); err != nil {
            return err
        }

        // Publish event (saved to outbox in same transaction)
        publisher := publisherFactory.Create(store)
        return bookspb.NewEventPublisher(publisher).PublishBookCreatedEvent(ctx,
            &bookspb.BookCreatedEvent{
                Id:     book.ID,
                Title:  book.Title,
                Author: book.Author,
            },
        )
    })
}
```

#### Message Relay

```go
package main

import (
    "context"
    "log"
    "time"

    "github.com/quarks-tech/amqpx"
    "github.com/quarks-tech/protoevent-go/pkg/transport/outbox"
    "github.com/quarks-tech/protoevent-go/pkg/transport/rabbitmq"
)

func main() {
    ctx := context.Background()

    // RabbitMQ sender as downstream transport
    amqpClient := amqpx.NewClient(&amqpx.Config{
        Host:     "localhost",
        Port:     5672,
        Username: "guest",
        Password: "guest",
    })
    sender := rabbitmq.NewSender(amqpClient)

    // Create relay
    relay := outbox.NewRelay(relayStore, sender,
        outbox.WithBatchSize(100),
        outbox.WithPollInterval(time.Second),
        outbox.WithBatchProcessing(),
        outbox.WithProcessingMode(outbox.ProcessingModeDelete),
        outbox.WithRetentionHours(2), // keep last 2 hours, truncate older partitions
    )

    // Run relay (blocks until context cancelled)
    if err := relay.Run(ctx); err != nil {
        log.Fatal(err)
    }
}
```

## SQL Schema for Outbox (TiDB)

```sql
CREATE TABLE outbox
(
  id          VARCHAR(36),                               -- UUID v7 (time-sortable)
  metadata    JSON                             NOT NULL, -- CloudEvents metadata
  data        VARBINARY(your-max-message-size) NOT NULL, -- Serialized event payload
  create_time DATETIME NOT NULL,
  sent_time   DATETIME,                                  -- NULL until relayed
  PRIMARY KEY (id, create_time)                          -- create_time required for partitioning
)
PARTITION BY HASH (HOUR(create_time)) PARTITIONS 24;    -- p0-p23 for each hour

CREATE TABLE outbox_cursor
(
  cursor VARCHAR(36) NOT NULL       -- last processed message ID
);

-- Optional: for monitoring/cleanup queries
CREATE INDEX idx_outbox_sent_time ON outbox (sent_time);
```

### Typical Queries

**Store (transactional):**
```sql
-- CreateMessage
INSERT INTO outbox (id, metadata, data, create_time) VALUES (?, ?, ?, ?);
```

**RelayStore:**
```sql
-- GetOutboxCursor: get stored cursor or first pending message ID
SELECT cursor FROM outbox_cursor LIMIT 1;
-- fallback if no cursor stored:
SELECT id FROM outbox WHERE sent_time IS NULL ORDER BY id LIMIT 1;

-- SaveOutboxCursor: persist cursor position (single row table)
DELETE FROM outbox_cursor;
INSERT INTO outbox_cursor (cursor) VALUES (?);

-- ListPendingOutboxMessages: paginate through pending messages
SELECT id, metadata, data, create_time, sent_time FROM outbox WHERE id >= ? AND sent_time IS NULL ORDER BY id LIMIT ?;

-- UpdateOutboxMessagesSentTime: mark as sent (ProcessingModeMarkSent)
UPDATE outbox SET sent_time = ? WHERE id IN (?...);

-- DeleteOutboxMessages: remove after sending (ProcessingModeDelete)
DELETE FROM outbox WHERE id IN (?...);

-- TruncateOutboxPartitions: fast cleanup of hourly partitions (p0-p23)
ALTER TABLE outbox TRUNCATE PARTITION p0, p1, p2;  -- partitions passed as arguments
```

**Operational (optional):**
```sql
-- Count pending messages
SELECT COUNT(*) FROM outbox WHERE sent_time IS NULL;

-- Oldest pending message age
SELECT MIN(create_time) FROM outbox WHERE sent_time IS NULL;

-- Truncate old partitions (instant cleanup)
-- Example: if current hour is 11 and retention is 2 hours, truncate p0-p8
-- Keep p9, p10, p11 (current hour and 2 hours back)
ALTER TABLE outbox TRUNCATE PARTITION p0, p1, p2, p3, p4, p5, p6, p7, p8;
```

## License

MIT
