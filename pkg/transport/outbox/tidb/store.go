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

// Store implements the outbox publish path and the relay read/offset/sequencer/
// retention/leader contracts over TiDB.
type Store struct {
	r  Runner
	db *sql.DB // non-nil only when built via NewStoreDB; needed by SequenceMessages
}

func NewStore(r Runner) *Store { return &Store{r: r} }

// NewStoreDB builds a store over a *sql.DB, enabling the sequencer, leader, and
// retention paths (which manage their own transactions / run on the pool).
func NewStoreDB(db *sql.DB) *Store { return &Store{r: db, db: db} }

var (
	_ outbox.Store            = (*Store)(nil)
	_ sequence.Store          = (*Store)(nil)
	_ sequence.SequencerStore = (*Store)(nil)
	_ sequence.RetentionStore = (*Store)(nil)
	_ relay.LeaderStore       = (*Store)(nil)
)

// CreateOutboxMessage inserts an unsequenced row. tx_start_ts is the publishing
// transaction's PD start TSO (@@tidb_current_ts); id is auto-assigned in insert
// order (= emit order); seq stays NULL until the sequencer runs. Call this on a
// transaction-scoped Runner so the row commits atomically with business writes.
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
	_, err = s.r.ExecContext(ctx, `
INSERT INTO outbox (seq, tx_start_ts, event_id, metadata, data, create_time, occurred_at)
VALUES (NULL, @@tidb_current_ts, ?, ?, ?, ?, ?)`,
		id[:], meta, m.Data, m.CreateTime.UTC(), md.Time.UTC(),
	)
	if err != nil {
		return fmt.Errorf("outbox: insert: %w", err)
	}
	return nil
}

// ListMessages returns sequenced rows with seq > afterSeq in seq order.
func (s *Store) ListMessages(ctx context.Context, afterSeq int64, limit int) ([]*outbox.Message, error) {
	rows, err := s.r.QueryContext(ctx, `
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
		var createTime = new(sqlTime)
		if err := rows.Scan(&seq, &eventID, &meta, &data, createTime); err != nil {
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
			CreateTime: createTime.t,
		})
	}
	return out, rows.Err()
}

// Offset returns the named consumer's watermark (0 if unset).
func (s *Store) Offset(ctx context.Context, name string) (int64, error) {
	var seq int64
	err := s.r.QueryRowContext(ctx, `SELECT last_seq FROM outbox_offsets WHERE name = ?`, name).Scan(&seq)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("outbox: offset: %w", err)
	}
	return seq, nil
}

// CommitOffset advances the watermark monotonically (GREATEST).
func (s *Store) CommitOffset(ctx context.Context, name string, seq int64) error {
	_, err := s.r.ExecContext(ctx, `
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
func (s *Store) InitOffsetLatest(ctx context.Context, name string) (int64, error) {
	_, err := s.r.ExecContext(ctx, `
INSERT INTO outbox_offsets (name, last_seq, update_time)
SELECT ?, COALESCE(MAX(seq), 0), NOW(6) FROM outbox
ON DUPLICATE KEY UPDATE
    last_seq    = GREATEST(last_seq, VALUES(last_seq)),
    update_time = VALUES(update_time)`, name)
	if err != nil {
		return 0, fmt.Errorf("outbox: init offset latest: %w", err)
	}
	var seq int64
	if err := s.r.QueryRowContext(ctx,
		`SELECT last_seq FROM outbox_offsets WHERE name = ?`, name,
	).Scan(&seq); err != nil {
		return 0, fmt.Errorf("outbox: init offset latest read back: %w", err)
	}
	return seq, nil
}

// SequenceMessages assigns dense seq values to committed pending rows in
// (tx_start_ts, id) order. The counter row is locked FOR UPDATE for the whole
// pass, so concurrent sequencers serialize and can never double-assign.
func (s *Store) SequenceMessages(ctx context.Context, limit int) (int, error) {
	if s.db == nil {
		return 0, fmt.Errorf("outbox: SequenceMessages requires a *sql.DB store (use NewStoreDB)")
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{})
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
func (s *Store) TryAcquireLeaderLock(ctx context.Context, name, holderID string, ttl time.Duration) (bool, error) {
	if _, err := s.r.ExecContext(ctx, `
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
	if err := s.r.QueryRowContext(ctx,
		`SELECT holder_id FROM relay_lock WHERE name = ?`, name,
	).Scan(&holder); err != nil {
		return false, fmt.Errorf("outbox: read lock holder: %w", err)
	}
	return holder == holderID, nil
}

// ReleaseLeaderLock drops the lock if still held by holderID.
func (s *Store) ReleaseLeaderLock(ctx context.Context, name, holderID string) error {
	_, err := s.r.ExecContext(ctx,
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
func (s *Store) SweepMessages(ctx context.Context, before time.Time, limit int) (int, error) {
	res, err := s.r.ExecContext(ctx, `
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

// sqlTime scans a DATETIME(6). The DSN must set parseTime=true (see tidbtest).
type sqlTime struct{ t time.Time }

func (s *sqlTime) Scan(v any) error {
	switch x := v.(type) {
	case time.Time:
		s.t = x
	case nil:
		s.t = time.Time{}
	default:
		return fmt.Errorf("outbox: cannot scan %T into time", v)
	}
	return nil
}
