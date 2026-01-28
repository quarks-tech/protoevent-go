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

#### Architecture

The relay uses a **multi-table approach** that eliminates cursor management and prevents race conditions:

```
┌─────────────────┐     send ok      ┌─────────────────┐
│ outbox_pending  │ ───────────────► │outbox_completed │
│  (to be sent)   │     (delete or   │   (optional)    │
└────────┬────────┘       move)      └─────────────────┘
         │
         │ send failed
         ▼
┌─────────────────┐  retry_time passed  ┌─────────────────┐
│   outbox_wait   │ ◄──────────────────►│ outbox_pending  │
│  (retry queue)  │                     │                 │
└────────┬────────┘                     └─────────────────┘
         │
         │ max retries exceeded
         ▼
┌─────────────────┐
│outbox_parking_lot│
│  (dead letter)  │
└─────────────────┘
```

**Benefits:**
- **No cursor management** - always read from beginning of pending table
- **No race conditions** - messages are never skipped, even with concurrent UUID v7 generation
- **Simpler queries** - no cursor comparisons needed
- **Smaller working set** - pending table stays small

#### Implement Store Interfaces (TiDB)

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

// Store implements outbox.Store for transactional operations.
// Embed this in your application store to use within transactions.
type Store struct {
    db *sql.DB
}

func (s *Store) CreateOutboxMessage(ctx context.Context, msg *outbox.Message) error {
    metadata, err := json.Marshal(msg.Metadata)
    if err != nil {
        return fmt.Errorf("marshal metadata: %w", err)
    }

    _, err = s.db.ExecContext(ctx, `
        INSERT INTO outbox_pending (id, metadata, data, create_time)
        VALUES (?, ?, ?, ?)
    `, msg.ID, metadata, msg.Data, msg.CreateTime)
    return err
}

// RelayStore implements relay.Store for relay operations.
type RelayStore struct {
    db *sql.DB
}

func NewRelayStore(db *sql.DB) *RelayStore {
    return &RelayStore{db: db}
}

func (s *RelayStore) ListPendingMessages(ctx context.Context, limit int) ([]*outbox.Message, error) {
    query := `SELECT id, metadata, data, create_time FROM outbox_pending ORDER BY id LIMIT ?`
    rows, err := s.db.QueryContext(ctx, query, limit)
    if err != nil {
        return nil, err
    }
    defer rows.Close()

    var messages []*outbox.Message
    for rows.Next() {
        var (
            id           []byte
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
            ID:         string(id),
            Metadata:   &metadata,
            Data:       data,
            CreateTime: createTime,
        })
    }
    return messages, rows.Err()
}

func (s *RelayStore) DeletePendingMessages(ctx context.Context, ids ...string) error {
    if len(ids) == 0 {
        return nil
    }

    placeholders := make([]string, len(ids))
    args := make([]any, 0, len(ids))
    for i, id := range ids {
        placeholders[i] = "?"
        args = append(args, id)
    }

    query := fmt.Sprintf(`DELETE FROM outbox_pending WHERE id IN (%s)`, strings.Join(placeholders, ","))
    _, err := s.db.ExecContext(ctx, query, args...)
    return err
}

func (s *RelayStore) MovePendingToCompleted(ctx context.Context, sentTime time.Time, ids ...string) error {
    if len(ids) == 0 {
        return nil
    }

    tx, err := s.db.BeginTx(ctx, nil)
    if err != nil {
        return err
    }
    defer tx.Rollback()

    placeholders := make([]string, len(ids))
    args := make([]any, 0, len(ids)+1)
    args = append(args, sentTime)
    for i, id := range ids {
        placeholders[i] = "?"
        args = append(args, id)
    }

    insertQuery := fmt.Sprintf(`
        INSERT INTO outbox_completed (id, metadata, data, create_time, sent_time)
        SELECT id, metadata, data, create_time, ? FROM outbox_pending WHERE id IN (%s)
    `, strings.Join(placeholders, ","))
    if _, err := tx.ExecContext(ctx, insertQuery, args...); err != nil {
        return err
    }

    deleteQuery := fmt.Sprintf(`DELETE FROM outbox_pending WHERE id IN (%s)`, strings.Join(placeholders, ","))
    if _, err := tx.ExecContext(ctx, deleteQuery, args[1:]...); err != nil {
        return err
    }

    return tx.Commit()
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
    "database/sql"
    "log"
    "time"

    "github.com/quarks-tech/amqpx"
    "github.com/quarks-tech/protoevent-go/pkg/transport/outbox/relay"
    "github.com/quarks-tech/protoevent-go/pkg/transport/rabbitmq"

    "yourapp/storage" // your storage package with RelayStore implementation
)

func main() {
    ctx := context.Background()

    // Database connection
    db, err := sql.Open("mysql", "user:pass@tcp(localhost:4000)/mydb")
    if err != nil {
        log.Fatal(err)
    }
    defer db.Close()

    // RabbitMQ sender as downstream transport
    amqpClient := amqpx.NewClient(&amqpx.Config{
        Host:     "localhost",
        Port:     5672,
        Username: "guest",
        Password: "guest",
    })
    defer amqpClient.Close()
    sender := rabbitmq.NewSender(amqpClient)

    // Create relay store
    relayStore := storage.NewRelayStore(db)

    // Create relay
    r := relay.NewRelay(relayStore, sender,
        relay.WithBatchSize(100),
        relay.WithPollInterval(time.Second),
        relay.WithProcessingMode(relay.ProcessingModeDelete), // or ProcessingModeMove for audit trail
    )

    // Run relay (blocks until context cancelled)
    if err := r.Run(ctx); err != nil {
        log.Fatal(err)
    }
}
```

#### Message Relay with Leader Election

For running multiple relay instances with automatic failover, enable leader election.
Only one instance will be active at a time, ensuring strict FIFO ordering.
All pods use identical configuration - the leader is elected automatically via database lock.

```go
// Create relay with leader election
// relayStore must implement relay.LeaderStore interface
r := relay.NewRelay(relayStore, sender,
    relay.WithBatchSize(100),
    relay.WithPollInterval(time.Second),
    relay.WithLeaderElection("outbox-relay"), // lock name
    relay.WithLeaseTTL(30*time.Second),       // lock expires after 30s if not renewed
)

// Run relay (blocks until context cancelled)
// Only the leader instance will process messages
if err := r.Run(ctx); err != nil {
    log.Fatal(err)
}
```

#### Message Relay with Parking Lot (Retry + Dead Letter)

For automatic retry with backoff and dead letter support, use the parkinglot relay.
Failed messages are moved to a wait table, then retried after backoff.
After max retries, messages are moved to the parking lot for manual inspection.

```go
import (
    "github.com/quarks-tech/protoevent-go/pkg/transport/outbox/relay"
    "github.com/quarks-tech/protoevent-go/pkg/transport/outbox/relay/parkinglot"
)

// Create relay with parking lot support
// relayStore must implement parkinglot.Store interface
r := parkinglot.NewRelay(relayStore, sender,
    parkinglot.WithBatchSize(100),
    parkinglot.WithPollInterval(time.Second),
    parkinglot.WithMaxRetries(3),
    parkinglot.WithMinRetryBackoff(15*time.Second),
    parkinglot.WithProcessingMode(relay.ProcessingModeDelete), // or relay.ProcessingModeMove
    parkinglot.WithLeaderElection("outbox-relay-pl"),          // optional
)

if err := r.Run(ctx); err != nil {
    log.Fatal(err)
}
```

#### Partition Cleanup (Optional)

When using `ProcessingModeMove` to keep an audit trail, the completed table can grow large.
Use `PartitionCleaner` alongside the relay in an errgroup for automatic cleanup of old partitions.

Requires the completed table to be partitioned by `HASH(HOUR(sent_time)) PARTITIONS 24`.

```go
import (
    "golang.org/x/sync/errgroup"
    "github.com/quarks-tech/protoevent-go/pkg/transport/outbox/relay"
)

// relayStore must implement relay.PartitionedStore interface
r := relay.NewRelay(relayStore, sender,
    relay.WithProcessingMode(relay.ProcessingModeMove), // keep audit trail
)

cleaner := relay.NewPartitionCleaner(relayStore,
    relay.WithRetentionHours(2),          // keep last 2 hours
    relay.WithCheckInterval(time.Hour),   // check once per hour
)

// Run both in an errgroup
g, ctx := errgroup.WithContext(ctx)
g.Go(func() error { return r.Run(ctx) })
g.Go(func() error { return cleaner.Run(ctx) })

if err := g.Wait(); err != nil {
    log.Fatal(err)
}
```

#### Implement Parking Lot Store (TiDB)

```go
// ParkingLotStore implements parkinglot.Store for the four-table approach.
type ParkingLotStore struct {
    db *sql.DB
}

func NewParkingLotStore(db *sql.DB) *ParkingLotStore {
    return &ParkingLotStore{db: db}
}

func (s *ParkingLotStore) ListPendingMessages(ctx context.Context, limit int) ([]*outbox.Message, error) {
    query := `SELECT id, metadata, data, create_time, retry_count FROM outbox_pending ORDER BY id LIMIT ?`
    rows, err := s.db.QueryContext(ctx, query, limit)
    if err != nil {
        return nil, err
    }
    defer rows.Close()

    var messages []*outbox.Message
    for rows.Next() {
        var (
            id           []byte
            metadataJSON []byte
            data         []byte
            createTime   time.Time
            retryCount   int
        )
        if err := rows.Scan(&id, &metadataJSON, &data, &createTime, &retryCount); err != nil {
            return nil, err
        }

        var metadata event.Metadata
        if err := json.Unmarshal(metadataJSON, &metadata); err != nil {
            return nil, fmt.Errorf("unmarshal metadata: %w", err)
        }

        messages = append(messages, &outbox.Message{
            ID:         string(id),
            Metadata:   &metadata,
            Data:       data,
            CreateTime: createTime,
            RetryCount: retryCount,
        })
    }
    return messages, rows.Err()
}

func (s *ParkingLotStore) DeletePendingMessages(ctx context.Context, ids ...string) error {
    if len(ids) == 0 {
        return nil
    }
    placeholders := make([]string, len(ids))
    args := make([]any, len(ids))
    for i, id := range ids {
        placeholders[i] = "?"
        args[i] = id
    }
    query := fmt.Sprintf(`DELETE FROM outbox_pending WHERE id IN (%s)`, strings.Join(placeholders, ","))
    _, err := s.db.ExecContext(ctx, query, args...)
    return err
}

func (s *ParkingLotStore) MovePendingToCompleted(ctx context.Context, sentTime time.Time, ids ...string) error {
    if len(ids) == 0 {
        return nil
    }
    tx, err := s.db.BeginTx(ctx, nil)
    if err != nil {
        return err
    }
    defer tx.Rollback()

    placeholders := make([]string, len(ids))
    args := make([]any, len(ids)+1)
    args[0] = sentTime
    for i, id := range ids {
        placeholders[i] = "?"
        args[i+1] = id
    }

    insertQuery := fmt.Sprintf(`
        INSERT INTO outbox_completed (id, metadata, data, create_time, sent_time)
        SELECT id, metadata, data, create_time, ? FROM outbox_pending WHERE id IN (%s)
    `, strings.Join(placeholders, ","))
    if _, err := tx.ExecContext(ctx, insertQuery, args...); err != nil {
        return err
    }

    deleteQuery := fmt.Sprintf(`DELETE FROM outbox_pending WHERE id IN (%s)`, strings.Join(placeholders, ","))
    if _, err := tx.ExecContext(ctx, deleteQuery, args[1:]...); err != nil {
        return err
    }

    return tx.Commit()
}

// Note: GetPendingMessageRetryCount is no longer needed - retry count is populated
// by ListPendingMessages into Message.RetryCount

func (s *ParkingLotStore) MovePendingToWait(ctx context.Context, id string, retryTime time.Time, retryCount int) error {
    tx, err := s.db.BeginTx(ctx, nil)
    if err != nil {
        return err
    }
    defer tx.Rollback()

    _, err = tx.ExecContext(ctx, `
        INSERT INTO outbox_wait (id, metadata, data, create_time, retry_count, retry_time)
        SELECT id, metadata, data, create_time, ?, ? FROM outbox_pending WHERE id = ?
    `, retryCount, retryTime, id)
    if err != nil {
        return err
    }

    _, err = tx.ExecContext(ctx, `DELETE FROM outbox_pending WHERE id = ?`, id)
    if err != nil {
        return err
    }

    return tx.Commit()
}

func (s *ParkingLotStore) MoveWaitToPending(ctx context.Context) error {
    tx, err := s.db.BeginTx(ctx, nil)
    if err != nil {
        return err
    }
    defer tx.Rollback()

    _, err = tx.ExecContext(ctx, `
        INSERT INTO outbox_pending (id, metadata, data, create_time, retry_count)
        SELECT id, metadata, data, create_time, retry_count FROM outbox_wait WHERE retry_time <= NOW()
    `)
    if err != nil {
        return err
    }

    _, err = tx.ExecContext(ctx, `DELETE FROM outbox_wait WHERE retry_time <= NOW()`)
    if err != nil {
        return err
    }

    return tx.Commit()
}

func (s *ParkingLotStore) MovePendingToParkingLot(ctx context.Context, id string, reason string) error {
    tx, err := s.db.BeginTx(ctx, nil)
    if err != nil {
        return err
    }
    defer tx.Rollback()

    _, err = tx.ExecContext(ctx, `
        INSERT INTO outbox_parking_lot (id, metadata, data, create_time, retry_count, reason, park_time)
        SELECT id, metadata, data, create_time, retry_count, ?, NOW() FROM outbox_pending WHERE id = ?
    `, reason, id)
    if err != nil {
        return err
    }

    _, err = tx.ExecContext(ctx, `DELETE FROM outbox_pending WHERE id = ?`, id)
    if err != nil {
        return err
    }

    return tx.Commit()
}
```

#### Implement LeaderStore (TiDB)

```go
func (s *RelayStore) TryAcquireLeaderLock(ctx context.Context, name, holderID string, leaseTTL time.Duration) (bool, error) {
    holderUUID, err := uuid.Parse(holderID)
    if err != nil {
        return false, fmt.Errorf("parse holder ID: %w", err)
    }
    holderBytes := holderUUID[:]
    expireTime := time.Now().Add(leaseTTL)

    // Try to acquire or renew the lock
    _, err = s.db.ExecContext(ctx, `
        INSERT INTO relay_lock (name, holder_id, expire_time) VALUES (?, ?, ?)
        ON DUPLICATE KEY UPDATE
            holder_id = CASE
                WHEN expire_time < NOW() THEN VALUES(holder_id)
                WHEN holder_id = VALUES(holder_id) THEN holder_id
                ELSE holder_id
            END,
            expire_time = CASE
                WHEN expire_time < NOW() THEN VALUES(expire_time)
                WHEN holder_id = VALUES(holder_id) THEN VALUES(expire_time)
                ELSE expire_time
            END
    `, name, holderBytes, expireTime)
    if err != nil {
        return false, fmt.Errorf("upsert lock: %w", err)
    }

    // Check if we hold the lock
    var currentHolder []byte
    err = s.db.QueryRowContext(ctx, `SELECT holder_id FROM relay_lock WHERE name = ?`, name).Scan(&currentHolder)
    if err != nil {
        return false, fmt.Errorf("check lock holder: %w", err)
    }

    return bytes.Equal(currentHolder, holderBytes), nil
}
```

#### Implement PartitionedStore (TiDB)

```go
func (s *RelayStore) TruncateCompletedPartitions(ctx context.Context, partitions ...int) error {
    if len(partitions) == 0 {
        return nil
    }

    // Build partition names: p0, p1, p2, ...
    names := make([]string, len(partitions))
    for i, p := range partitions {
        names[i] = fmt.Sprintf("p%d", p)
    }

    query := fmt.Sprintf("ALTER TABLE outbox_completed TRUNCATE PARTITION %s", strings.Join(names, ", "))
    _, err := s.db.ExecContext(ctx, query)
    return err
}
```

## SQL Schema for Outbox (TiDB)

The outbox uses a **multi-table approach** for simplicity and reliability:

```sql
-- Pending messages (messages waiting to be sent)
-- This table stays small as messages are deleted/moved after successful send
CREATE TABLE outbox_pending
(
  id          BINARY(16) PRIMARY KEY,                    -- UUID v7 (time-sortable)
  metadata    JSON                             NOT NULL, -- CloudEvents metadata
  data        VARBINARY(your-max-message-size) NOT NULL, -- Serialized event payload
  create_time DATETIME(6)                      NOT NULL,
  retry_count INT DEFAULT 0                              -- for parking lot relay
);

-- Completed messages (optional, for audit trail)
-- Use ProcessingModeMove to keep sent messages here
CREATE TABLE outbox_completed
(
  id          BINARY(16) PRIMARY KEY,
  metadata    JSON                             NOT NULL,
  data        VARBINARY(your-max-message-size) NOT NULL,
  create_time DATETIME(6)                      NOT NULL,
  sent_time   DATETIME(6)                      NOT NULL
)
  PARTITION BY HASH(HOUR(sent_time)) PARTITIONS 24; -- p0-p23 for fast cleanup

-- Wait queue (for parking lot relay - messages awaiting retry)
CREATE TABLE outbox_wait
(
  id          BINARY(16) PRIMARY KEY,
  metadata    JSON                             NOT NULL,
  data        VARBINARY(your-max-message-size) NOT NULL,
  create_time DATETIME(6)                      NOT NULL,
  retry_count INT                              NOT NULL,
  retry_time  DATETIME(6)                      NOT NULL  -- when to retry
);
CREATE INDEX idx_outbox_wait_retry_time ON outbox_wait (retry_time);

-- Parking lot (for parking lot relay - permanently failed messages)
CREATE TABLE outbox_parking_lot
(
  id          BINARY(16) PRIMARY KEY,
  metadata    JSON                             NOT NULL,
  data        VARBINARY(your-max-message-size) NOT NULL,
  create_time DATETIME(6)                      NOT NULL,
  retry_count INT                              NOT NULL,
  reason      TEXT                             NOT NULL, -- why message was parked
  park_time   DATETIME(6)                      NOT NULL  -- when message was parked
);

-- For leader election (optional, required for WithLeaderElection)
CREATE TABLE relay_lock
(
  name        VARCHAR(255) PRIMARY KEY, -- lock name (e.g., "outbox-relay")
  holder_id   BINARY(16) NOT NULL,      -- UUID of the current leader
  expire_time DATETIME(6) NOT NULL      -- lock expires after this time
);
```

### Typical Queries

**Store (transactional - within your business transaction):**
```sql
-- CreateOutboxMessage: insert new message into pending table
INSERT INTO outbox_pending (id, metadata, data, create_time) VALUES (?, ?, ?, ?);
```

**RelayStore:**
```sql
-- ListPendingMessages: get messages to process (always from beginning, no cursor!)
SELECT id, metadata, data, create_time FROM outbox_pending ORDER BY id LIMIT ?;

-- DeletePendingMessages: remove after successful send (ProcessingModeDelete)
DELETE FROM outbox_pending WHERE id IN (?...);

-- MovePendingToCompleted: move to completed table (ProcessingModeMove)
INSERT INTO outbox_completed (id, metadata, data, create_time, sent_time)
SELECT id, metadata, data, create_time, ? FROM outbox_pending WHERE id IN (?...);
DELETE FROM outbox_pending WHERE id IN (?...);
```

**ParkingLotStore:**
```sql
-- MovePendingToWait: move failed message to wait queue
INSERT INTO outbox_wait (id, metadata, data, create_time, retry_count, retry_time)
SELECT id, metadata, data, create_time, ?, ? FROM outbox_pending WHERE id = ?;
DELETE FROM outbox_pending WHERE id = ?;

-- MoveWaitToPending: move messages back when retry time passes
INSERT INTO outbox_pending (id, metadata, data, create_time, retry_count)
SELECT id, metadata, data, create_time, retry_count FROM outbox_wait WHERE retry_time <= NOW();
DELETE FROM outbox_wait WHERE retry_time <= NOW();

-- MovePendingToParkingLot: move to parking lot after max retries
INSERT INTO outbox_parking_lot (id, metadata, data, create_time, retry_count, reason, park_time)
SELECT id, metadata, data, create_time, retry_count, ?, NOW() FROM outbox_pending WHERE id = ?;
DELETE FROM outbox_pending WHERE id = ?;
```

**LeaderStore (optional):**
```sql
-- TryAcquireLeaderLock: acquire or renew leader lock
INSERT INTO relay_lock (name, holder_id, expire_time) VALUES (?, ?, ?)
ON DUPLICATE KEY UPDATE
    holder_id = CASE
        WHEN expire_time < NOW() THEN VALUES(holder_id)
        WHEN holder_id = VALUES(holder_id) THEN holder_id
        ELSE holder_id
    END,
    expire_time = CASE
        WHEN expire_time < NOW() THEN VALUES(expire_time)
        WHEN holder_id = VALUES(holder_id) THEN VALUES(expire_time)
        ELSE expire_time
    END;

-- Check current lock holder
SELECT holder_id FROM relay_lock WHERE name = ?;
```

**PartitionedStore (optional - for partition cleanup):**
```sql
-- TruncateCompletedPartitions: fast cleanup of hourly partitions (p0-p23)
-- Example: truncate partitions outside retention window
ALTER TABLE outbox_completed TRUNCATE PARTITION p0, p1, p2, p3, p4, p5, p6, p7, p8;
```

**Operational (monitoring):**
```sql
-- Count pending messages
SELECT COUNT(*) FROM outbox_pending;

-- Oldest pending message age
SELECT MIN(create_time) FROM outbox_pending;

-- Messages in wait queue
SELECT COUNT(*) FROM outbox_wait;

-- Messages in parking lot (need manual intervention)
SELECT COUNT(*) FROM outbox_parking_lot;
```

## License

MIT
