package tidb

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/quarks-tech/protoevent-go/pkg/event"
	"github.com/quarks-tech/protoevent-go/pkg/transport/outbox"
	"github.com/quarks-tech/protoevent-go/pkg/transport/outbox/relay"
	"github.com/quarks-tech/protoevent-go/pkg/transport/outbox/relay/sequence"
)

// Runner is the subset of *sql.DB / *sql.Tx the store needs. Publish uses a
// tx-scoped Runner (atomic with business writes); the relay uses *sql.DB.
type Runner interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// Store is the tx-scoped publish store: it implements only
// CreateOutboxMessage, over a Runner that is typically a transaction-scoped
// *sql.Tx so the outbox row commits atomically with business writes.
type Store struct {
	r Runner
}

// NewStore builds a publish-side store over r, typically a transaction-scoped
// *sql.Tx for atomic publish. r may also be a *sql.DB for a fire-and-forget
// publish outside a business transaction. For relay use (read/offset/
// sequencer/retention/leader), use NewRelayStore.
func NewStore(r Runner) *Store { return &Store{r: r} }

var _ outbox.Store = (*Store)(nil)

// RelayStore is the pool-backed relay store: it embeds a publish-side Store
// (built over db, so it can also publish) plus everything the relay
// runtimes need — read/offset, the sequencer, the retention sweep, and leader
// election. These all manage their own transactions against the pool, so they
// require a *sql.DB rather than a transaction-scoped Runner.
type RelayStore struct {
	*Store
	db *sql.DB
}

// NewRelayStore builds a relay store over db.
func NewRelayStore(db *sql.DB) *RelayStore { return &RelayStore{Store: NewStore(db), db: db} }

var (
	_ sequence.Store          = (*RelayStore)(nil)
	_ sequence.SequencerStore = (*RelayStore)(nil)
	_ sequence.RetentionStore = (*RelayStore)(nil)
	_ relay.LeaderStore       = (*RelayStore)(nil)
)

// CreateOutboxMessage inserts an unsequenced row. tx_start_ts is the publishing
// transaction's PD start TSO (@@tidb_current_ts); id is auto-assigned in insert
// order (= emit order); seq stays NULL until the sequencer runs. Call this on a
// transaction-scoped Runner so the row commits atomically with business writes.
//
// Requires Message.ID to be a UUID string: event_id is a BINARY(16) column, and
// m.ID is parsed as a UUID before the insert. A custom outbox.IDGenerator
// (via outbox.WithIDGenerator) MUST emit UUIDs to be usable with this store.
func (s *Store) CreateOutboxMessage(ctx context.Context, m *outbox.Message) error {
	id, err := uuid.Parse(m.ID)
	if err != nil {
		return fmt.Errorf("outbox: parse message ID %q: %w", m.ID, err)
	}
	md := m.Metadata
	meta, err := json.Marshal(md)
	if err != nil {
		return fmt.Errorf("outbox: marshal metadata: %w", err)
	}
	// occurred_at is kept as a queryable/indexable event-time column for
	// operators and future event-time features; the engine itself reads event
	// time from the metadata JSON, and retention (SweepMessages) is
	// create_time-anchored, not occurred_at-anchored.
	_, err = s.r.ExecContext(ctx, `
INSERT INTO outbox (seq, tx_start_ts, event_id, metadata, data, create_time, occurred_at)
VALUES (NULL, @@tidb_current_ts, ?, ?, ?, ?, ?)`,
		id[:], meta, m.Data, m.CreateTime, md.Time.UTC(),
	)
	if err != nil {
		return fmt.Errorf("outbox: insert: %w", err)
	}
	return nil
}

// ListMessages returns sequenced rows with seq > afterSeq in seq order.
func (rs *RelayStore) ListMessages(ctx context.Context, afterSeq int64, limit int) ([]*outbox.Message, error) {
	rows, err := rs.db.QueryContext(ctx, `
SELECT seq, event_id, metadata, data, create_time
FROM outbox
WHERE seq > ?
ORDER BY seq
LIMIT ?`, afterSeq, limit)
	if err != nil {
		return nil, fmt.Errorf("outbox: list: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []*outbox.Message
	for rows.Next() {
		var (
			seq     int64
			eventID []byte
			meta    []byte
			data    []byte
		)
		var createTime time.Time
		if err := rows.Scan(&seq, &eventID, &meta, &data, &createTime); err != nil {
			return nil, fmt.Errorf("outbox: scan: %w", err)
		}
		id, err := uuid.FromBytes(eventID)
		if err != nil {
			return nil, fmt.Errorf("outbox: event_id not a uuid: %w", err)
		}
		var md event.Metadata
		if err := json.Unmarshal(meta, &md); err != nil {
			return nil, fmt.Errorf("outbox: unmarshal metadata: %w", err)
		}
		out = append(out, &outbox.Message{
			ID:         id.String(),
			Seq:        seq,
			Metadata:   &md,
			Data:       data,
			CreateTime: createTime,
		})
	}
	return out, rows.Err()
}

// Offset returns the named consumer's watermark (0 if unset).
func (rs *RelayStore) Offset(ctx context.Context, name string) (int64, error) {
	var seq int64
	err := rs.db.QueryRowContext(ctx, `SELECT last_seq FROM outbox_offsets WHERE name = ?`, name).Scan(&seq)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("outbox: offset: %w", err)
	}
	return seq, nil
}

// CommitOffset advances the watermark monotonically (GREATEST).
func (rs *RelayStore) CommitOffset(ctx context.Context, name string, seq int64) error {
	_, err := rs.db.ExecContext(ctx, `
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

// InitOffsetLatest is called once for a consumer group with no committed
// offset: it atomically initializes the group's offset row to the current
// maximum assigned seq (0 if the log is empty or unsequenced) and returns the
// effective offset. Monotone (GREATEST) so it never rewinds an existing row.
func (rs *RelayStore) InitOffsetLatest(ctx context.Context, name string) (int64, error) {
	_, err := rs.db.ExecContext(ctx, `
INSERT INTO outbox_offsets (name, last_seq, update_time)
SELECT ?, COALESCE(MAX(seq), 0), NOW(6) FROM outbox
ON DUPLICATE KEY UPDATE
    last_seq    = GREATEST(last_seq, VALUES(last_seq)),
    update_time = VALUES(update_time)`, name)
	if err != nil {
		return 0, fmt.Errorf("outbox: init offset latest: %w", err)
	}
	var seq int64
	if err := rs.db.QueryRowContext(ctx,
		`SELECT last_seq FROM outbox_offsets WHERE name = ?`, name,
	).Scan(&seq); err != nil {
		return 0, fmt.Errorf("outbox: init offset latest read back: %w", err)
	}
	return seq, nil
}

// SequenceMessages assigns dense seq values to committed pending rows in
// (tx_start_ts, id) order. The counter row is locked FOR UPDATE for the whole
// pass, so concurrent sequencers serialize and can never double-assign.
func (rs *RelayStore) SequenceMessages(ctx context.Context, limit int) (int, error) {
	tx, err := rs.db.BeginTx(ctx, &sql.TxOptions{})
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
func (rs *RelayStore) TryAcquireLeaderLock(ctx context.Context, name, holderID string, ttl time.Duration) (bool, error) {
	if _, err := rs.db.ExecContext(ctx, `
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
	if err := rs.db.QueryRowContext(ctx,
		`SELECT holder_id FROM relay_lock WHERE name = ?`, name,
	).Scan(&holder); err != nil {
		return false, fmt.Errorf("outbox: read lock holder: %w", err)
	}
	return holder == holderID, nil
}

// ReleaseLeaderLock drops the lock if still held by holderID.
func (rs *RelayStore) ReleaseLeaderLock(ctx context.Context, name, holderID string) error {
	_, err := rs.db.ExecContext(ctx,
		`DELETE FROM relay_lock WHERE name = ? AND holder_id = ?`, name, holderID)
	if err != nil {
		return fmt.Errorf("outbox: release lock: %w", err)
	}
	return nil
}

// SweepMessages deletes sequenced rows at or below the minimum committed offset
// across all consumers and inserted (create_time) before `before`, bounded to
// `limit`. Retention is anchored to insert time, not event time, so a
// backdated WithEventTime event is not swept early. If no offsets exist yet,
// MIN(last_seq) is NULL and nothing is deleted.
//
// MIN(last_seq) spans only registered offset rows: a consumer group that was
// created but never run (or InitOffsetLatest'd) has no outbox_offsets row and
// so provides no retention protection at all — an unrun group does not hold
// the sweep back. Consumer groups must run (or call InitOffsetLatest) within
// the retention window to be protected from the sweep.
func (rs *RelayStore) SweepMessages(ctx context.Context, before time.Time, limit int) (int, error) {
	res, err := rs.db.ExecContext(ctx, `
DELETE FROM outbox
WHERE seq IS NOT NULL
  AND seq <= (SELECT MIN(last_seq) FROM outbox_offsets)
  AND create_time < ?
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
