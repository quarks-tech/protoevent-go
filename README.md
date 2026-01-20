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
        INSERT INTO outbox_messages (id, metadata, data, create_time)
        VALUES (?, ?, ?, ?)
    `, msg.ID, metadata, msg.Data, msg.CreateTime)
    return err
}

// RelayStore implements relay.Store for relay operations.
// This is used by the relay to read and process outbox messages.
type RelayStore struct {
    db       *sql.DB
    cursorID string // identifier for this relay instance
}

func NewRelayStore(db *sql.DB, cursorID string) *RelayStore {
    return &RelayStore{db: db, cursorID: cursorID}
}

func (s *RelayStore) GetOutboxCursor(ctx context.Context) (string, error) {
    // Try to get stored cursor first
    var lastID sql.NullString
    err := s.db.QueryRowContext(ctx, `SELECT last_id FROM outbox_cursor WHERE id = ?`, s.cursorID).Scan(&lastID)
    if err == nil && lastID.Valid {
        return lastID.String, nil
    }
    if err != nil && err != sql.ErrNoRows {
        return "", err
    }

    // Fallback: get first pending message ID
    err = s.db.QueryRowContext(ctx, `
        SELECT id FROM outbox_messages WHERE sent_time IS NULL ORDER BY id LIMIT 1
    `).Scan(&lastID)
    if err == sql.ErrNoRows {
        return "", nil
    }
    return lastID.String, err
}

func (s *RelayStore) SaveOutboxCursor(ctx context.Context, cursor string) error {
    _, err := s.db.ExecContext(ctx, `
        INSERT INTO outbox_cursor (id, last_id) VALUES (?, ?)
        ON DUPLICATE KEY UPDATE last_id = VALUES(last_id)
    `, s.cursorID, cursor)
    return err
}

func (s *RelayStore) ListPendingOutboxMessages(ctx context.Context, cursor string, limit int) ([]*outbox.Message, error) {
    query := `
        SELECT id, metadata, data, create_time FROM outbox_messages
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

    query := fmt.Sprintf(`UPDATE outbox_messages SET sent_time = ? WHERE id IN (%s)`, strings.Join(placeholders, ","))
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

    query := fmt.Sprintf(`DELETE FROM outbox_messages WHERE id IN (%s)`, strings.Join(placeholders, ","))
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
    relayStore := storage.NewRelayStore(db, "outbox-relay")

    // Create relay
    r := relay.NewRelay(relayStore, sender,
        relay.WithBatchSize(100),
        relay.WithPollInterval(time.Second),
        relay.WithBatchProcessing(),
        relay.WithProcessingMode(relay.ProcessingModeDelete),
        relay.WithRetentionHours(2), // keep last 2 hours, truncate older partitions
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
Failed messages are moved to a wait queue, then retried after backoff.
After max retries, messages are moved to the parking lot for manual inspection.

```go
import "github.com/quarks-tech/protoevent-go/pkg/transport/outbox/relay/parkinglot"

// Create relay with parking lot support
// relayStore must implement parkinglot.Store interface
r := parkinglot.NewRelay(relayStore, sender,
    parkinglot.WithBatchSize(100),
    parkinglot.WithPollInterval(time.Second),
    parkinglot.WithMaxRetries(3),
    parkinglot.WithMinRetryBackoff(15*time.Second),
    parkinglot.WithLeaderElection("outbox-relay-pl"), // optional
)

if err := r.Run(ctx); err != nil {
    log.Fatal(err)
}
```

#### Implement Parking Lot Store (TiDB)

```go
func (s *RelayStore) IncrementOutboxMessageRetryCount(ctx context.Context, id string) (int, error) {
    // Single statement - returns new count via subsequent query
    _, err := s.db.ExecContext(ctx, `
        UPDATE outbox_messages SET retry_count = retry_count + 1 WHERE id = ?
    `, id)
    if err != nil {
        return 0, err
    }

    var count int
    err = s.db.QueryRowContext(ctx, `SELECT retry_count FROM outbox_messages WHERE id = ?`, id).Scan(&count)
    return count, err
}

func (s *RelayStore) GetOutboxMessageRetryCount(ctx context.Context, id string) (int, error) {
    var count int
    err := s.db.QueryRowContext(ctx, `SELECT retry_count FROM outbox_messages WHERE id = ?`, id).Scan(&count)
    return count, err
}

func (s *RelayStore) MoveOutboxMessageToWait(ctx context.Context, id string, retryTime time.Time) error {
    tx, err := s.db.BeginTx(ctx, nil)
    if err != nil {
        return err
    }
    defer tx.Rollback()

    _, err = tx.ExecContext(ctx, `
        INSERT INTO outbox_messages_wait (id, metadata, data, create_time, retry_count, retry_time)
        SELECT id, metadata, data, create_time, retry_count, ? FROM outbox_messages WHERE id = ?
    `, retryTime, id)
    if err != nil {
        return err
    }

    _, err = tx.ExecContext(ctx, `DELETE FROM outbox_messages WHERE id = ?`, id)
    if err != nil {
        return err
    }

    return tx.Commit()
}

func (s *RelayStore) MoveWaitingMessagesToOutbox(ctx context.Context) (int, error) {
    tx, err := s.db.BeginTx(ctx, nil)
    if err != nil {
        return 0, err
    }
    defer tx.Rollback()

    result, err := tx.ExecContext(ctx, `
        INSERT INTO outbox_messages (id, metadata, data, create_time, retry_count)
        SELECT id, metadata, data, create_time, retry_count FROM outbox_messages_wait WHERE retry_time <= NOW()
    `)
    if err != nil {
        return 0, err
    }

    _, err = tx.ExecContext(ctx, `DELETE FROM outbox_messages_wait WHERE retry_time <= NOW()`)
    if err != nil {
        return 0, err
    }

    if err := tx.Commit(); err != nil {
        return 0, err
    }

    count, _ := result.RowsAffected()
    return int(count), nil
}

func (s *RelayStore) MoveOutboxMessageToParkingLot(ctx context.Context, id string, reason string) error {
    tx, err := s.db.BeginTx(ctx, nil)
    if err != nil {
        return err
    }
    defer tx.Rollback()

    _, err = tx.ExecContext(ctx, `
        INSERT INTO outbox_messages_pl (id, metadata, data, create_time, retry_count, reason, park_time)
        SELECT id, metadata, data, create_time, retry_count, ?, NOW() FROM outbox_messages WHERE id = ?
    `, reason, id)
    if err != nil {
        return err
    }

    _, err = tx.ExecContext(ctx, `DELETE FROM outbox_messages WHERE id = ?`, id)
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

## SQL Schema for Outbox (TiDB)

```sql
CREATE TABLE outbox_messages
(
  id          BINARY(16),                                -- UUID v7 (time-sortable)
  metadata    JSON                             NOT NULL, -- CloudEvents metadata
  data        VARBINARY(your-max-message-size) NOT NULL, -- Serialized event payload
  create_time DATETIME                         NOT NULL,
  sent_time   DATETIME,                                  -- NULL until relayed
  retry_count INT DEFAULT 0,                             -- for parking lot relay
  PRIMARY KEY (id, create_time)                          -- create_time required for partitioning
)
  PARTITION BY HASH(HOUR(create_time)) PARTITIONS 24; -- p0-p23 for each hour

CREATE TABLE outbox_cursor
(
  id      VARCHAR(255) PRIMARY KEY, -- cursor identifier (e.g., relay instance name)
  last_id BINARY(16) NOT NULL       -- last processed message ID
);

-- For leader election (optional, required for WithLeaderElection)
CREATE TABLE relay_lock
(
  name        VARCHAR(255) PRIMARY KEY, -- lock name (e.g., "outbox-relay")
  holder_id   BINARY(16) NOT NULL,      -- UUID of the current leader
  expire_time DATETIME   NOT NULL       -- lock expires after this time
);

-- For parking lot relay (optional, required for parkinglot.Relay)
CREATE TABLE outbox_messages_wait
(
  id          BINARY(16) PRIMARY KEY,
  metadata    JSON                             NOT NULL,
  data        VARBINARY(your-max-message-size) NOT NULL,
  create_time DATETIME                         NOT NULL,
  retry_count INT                              NOT NULL,
  retry_time  DATETIME                         NOT NULL -- when to retry
);
CREATE
INDEX idx_outbox_messages_wait_retry_time ON outbox_messages_wait (retry_time);

CREATE TABLE outbox_messages_pl
(
  id          BINARY(16) PRIMARY KEY,
  metadata    JSON                             NOT NULL,
  data        VARBINARY(your-max-message-size) NOT NULL,
  create_time DATETIME                         NOT NULL,
  retry_count INT                              NOT NULL,
  reason      VARCHAR(256)                     NOT NULL, -- why message was parked
  park_time   DATETIME                         NOT NULL  -- when message was parked
);

-- Optional: for monitoring/cleanup queries
CREATE
INDEX idx_outbox_messages_sent_time ON outbox_messages (sent_time);
```

### Typical Queries

**Store (transactional):**
```sql
-- CreateMessage
INSERT INTO outbox_messages (id, metadata, data, create_time) VALUES (?, ?, ?, ?);
```

**RelayStore:**
```sql
-- GetOutboxCursor: get stored cursor for this relay instance
SELECT last_id FROM outbox_cursor WHERE id = ?;
-- fallback if no cursor stored:
SELECT id FROM outbox_messages WHERE sent_time IS NULL ORDER BY id LIMIT 1;

-- SaveOutboxCursor: persist cursor position (upsert)
INSERT INTO outbox_cursor (id, last_id) VALUES (?, ?)
ON DUPLICATE KEY UPDATE last_id = VALUES(last_id);

-- ListPendingOutboxMessages: paginate through pending messages
SELECT id, metadata, data, create_time, sent_time FROM outbox_messages WHERE id >= ? AND sent_time IS NULL ORDER BY id LIMIT ?;

-- UpdateOutboxMessagesSentTime: mark as sent (ProcessingModeMarkSent)
UPDATE outbox_messages SET sent_time = ? WHERE id IN (?...);

-- DeleteOutboxMessages: remove after sending (ProcessingModeDelete)
DELETE FROM outbox_messages WHERE id IN (?...);

-- TruncateOutboxPartitions: fast cleanup of hourly partitions (p0-p23)
ALTER TABLE outbox_messages TRUNCATE PARTITION p0, p1, p2;  -- partitions passed as arguments
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

**Operational (optional):**
```sql
-- Count pending messages
SELECT COUNT(*) FROM outbox_messages WHERE sent_time IS NULL;

-- Oldest pending message age
SELECT MIN(create_time) FROM outbox_messages WHERE sent_time IS NULL;

-- Truncate old partitions (instant cleanup)
-- Example: if current hour is 11 and retention is 2 hours, truncate p0-p8
-- Keep p9, p10, p11 (current hour and 2 hours back)
ALTER TABLE outbox_messages TRUNCATE PARTITION p0, p1, p2, p3, p4, p5, p6, p7, p8;
```

## License

MIT
