package tidb

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/quarks-tech/protoevent-go/pkg/event"
	"github.com/quarks-tech/protoevent-go/pkg/transport/outbox"
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
	r Runner
}

func NewStore(r Runner) *Store { return &Store{r: r} }

var (
	_ outbox.Store   = (*Store)(nil)
	_ sequence.Store = (*Store)(nil)
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
	_, err = s.r.ExecContext(ctx, `
INSERT INTO outbox (seq, tx_start_ts, event_id, `+"`type`"+`, source, subject, content_type, data, occurred_at)
VALUES (NULL, @@tidb_current_ts, ?, ?, ?, ?, ?, ?, ?)`,
		id[:], md.Type, md.Source, md.Subject, md.DataContentType, m.Data, md.Time.UTC(),
	)
	if err != nil {
		return fmt.Errorf("outbox: insert: %w", err)
	}
	return nil
}

// ListMessages returns sequenced rows with seq > afterSeq in seq order.
func (s *Store) ListMessages(ctx context.Context, afterSeq int64, limit int) ([]*outbox.Message, error) {
	rows, err := s.r.QueryContext(ctx, `
SELECT seq, event_id, `+"`type`"+`, source, subject, content_type, data, occurred_at
FROM outbox
WHERE seq > ?
ORDER BY seq
LIMIT ?`, afterSeq, limit)
	if err != nil {
		return nil, fmt.Errorf("outbox: list: %w", err)
	}
	defer rows.Close()

	var out []*outbox.Message
	for rows.Next() {
		var (
			seq                               int64
			eventID                           []byte
			typ, source, subject, contentType string
			data                              []byte
		)
		var occurredAt = new(sqlTime)
		if err := rows.Scan(&seq, &eventID, &typ, &source, &subject, &contentType, &data, occurredAt); err != nil {
			return nil, fmt.Errorf("outbox: scan: %w", err)
		}
		id, err := uuid.FromBytes(eventID)
		if err != nil {
			return nil, fmt.Errorf("outbox: event_id not a uuid: %w", err)
		}
		md := &event.Metadata{
			SpecVersion:     "1.0",
			ID:              id.String(),
			Type:            typ,
			Source:          source,
			Subject:         subject,
			DataContentType: contentType,
			Time:            occurredAt.t,
		}
		out = append(out, &outbox.Message{
			ID:         id.String(),
			Seq:        seq,
			Metadata:   md,
			Data:       data,
			CreateTime: occurredAt.t,
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
