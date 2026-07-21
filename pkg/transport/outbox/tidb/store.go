// Package tidb is the TiDB-backed outbox store: a transaction-scoped publish
// Store (NewStore) that commits outbox rows atomically with business writes,
// and a pool-backed RelayStore (NewRelayStore) implementing the relay and
// relay/sequence store contracts (read/offset, sequencer, retention sweep,
// leader election). Schema migrations are embedded as Migrations (see
// migrations/).
package tidb

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/google/uuid"

	"github.com/quarks-tech/protoevent-go/pkg/event"
	"github.com/quarks-tech/protoevent-go/pkg/transport/outbox"
	"github.com/quarks-tech/protoevent-go/pkg/transport/outbox/relay"
	"github.com/quarks-tech/protoevent-go/pkg/transport/outbox/relay/sequence"
)

// The four physical tables of one outbox instance. One prefix applies to all
// of them as a unit (WithTablePrefix): an outbox instance IS the four-table
// set — prefixing them together gives each instance its own log, offsets,
// sequencer counter, and leader locks, so several independent outboxes can
// coexist in one schema (and stay joinable with business writes in the same
// transaction, which is why a separate database is not an alternative on
// TiDB: publish SQL runs unqualified on the business transaction's schema).
const (
	baseMessagesTable   = "outbox_messages"
	baseOffsetsTable    = "outbox_offsets"
	baseSequencersTable = "outbox_sequencers"
	baseLocksTable      = "relay_locks"
)

// tablePrefixPattern constrains WithTablePrefix to a safe SQL identifier
// fragment: the prefix is spliced into DDL/DML as an IDENTIFIER (it cannot be
// a bound parameter), so it must never carry quoting or punctuation.
var tablePrefixPattern = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]*$`)

// maxTablePrefixLen keeps every prefixed identifier — including the
// per-instance golang-migrate versions table (prefix + "schema_migrations")
// — comfortably under TiDB's 64-character identifier limit.
const maxTablePrefixLen = 40

func validateTablePrefix(prefix string) error {
	if prefix == "" {
		return errors.New("outbox: table prefix must not be empty")
	}
	if len(prefix) > maxTablePrefixLen {
		return fmt.Errorf("outbox: table prefix %q exceeds %d characters", prefix, maxTablePrefixLen)
	}
	if !tablePrefixPattern.MatchString(prefix) {
		return fmt.Errorf("outbox: table prefix %q is not a valid identifier fragment (want %s)", prefix, tablePrefixPattern)
	}
	return nil
}

// config carries construction-time configuration shared by NewStore and
// NewRelayStore.
type config struct {
	prefix string
}

// Option configures a Store / RelayStore instance.
type Option func(*config)

// WithTablePrefix names this outbox instance: all four tables (and, by
// convention, the golang-migrate versions table — see PrefixedMigrations) get
// the prefix, letting several independent outboxes coexist in one schema,
// each with its own total order, retention, and consumer groups. The publish
// Store and the RelayStore of one instance MUST use the same prefix.
//
// The prefix is an SQL identifier fragment ([A-Za-z][A-Za-z0-9_]*, at most 40
// characters, e.g. "orders_"). An invalid prefix panics: it is static
// developer configuration, not runtime input — the regexp.MustCompile
// convention for programmer error.
func WithTablePrefix(prefix string) Option {
	if err := validateTablePrefix(prefix); err != nil {
		panic(err)
	}
	return func(c *config) { c.prefix = prefix }
}

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
	// insertQuery is rendered once at construction (the table name is an
	// identifier and cannot be a bound parameter; WithTablePrefix validates it).
	insertQuery string
}

// NewStore builds a publish-side store over r, which MUST be a
// transaction-scoped *sql.Tx: the row's tx_start_ts is the publishing
// transaction's PD start TSO (@@tidb_current_ts), which only exists on a
// transactional connection — on an autocommit *sql.DB it reads 0, and
// CreateOutboxMessage fails loudly rather than write a row that would sort
// before every transactional row in a sequencer batch. For relay use
// (read/offset/sequencer/retention/leader), use NewRelayStore.
//
// DSN note: with go-sql-driver/mysql, set interpolateParams=true — without it
// every parameterized query pays a prepare/exec/close cycle (3 wire round
// trips), including the publish INSERT inside the caller's business
// transaction. For relay use, parseTime=true is REQUIRED too (see
// NewRelayStore).
func NewStore(r Runner, opts ...Option) *Store {
	var c config
	for _, opt := range opts {
		opt(&c)
	}
	// occur_time is kept as a queryable/indexable event-time column for
	// operators and future event-time features; the engine itself reads event
	// time from the metadata JSON, and retention (SweepMessages) is
	// create_time-anchored, not occur_time-anchored.
	//
	// create_time = NOW(6), the DATABASE clock — deliberately NOT the
	// client-stamped Message.CreateTime: create_time is the retention anchor,
	// and a publisher host with a skewed clock would otherwise pin rows
	// forever (clock ahead) or expose them to an early sweep (clock behind).
	// SweepMessages' cutoff is NOW(6)-relative too, so retention lives
	// entirely in one clock domain.
	//
	// NULLIF(@@tidb_current_ts, 0): on an autocommit connection the variable
	// reads 0, and a 0 tx_start_ts would silently sort the row before every
	// transactional row in a sequencer batch. NULLIF turns that 0 into NULL so
	// the NOT NULL column rejects the row loudly instead.
	return &Store{r: r, insertQuery: fmt.Sprintf(`
INSERT INTO %s (seq, tx_start_ts, event_id, metadata, data, create_time, occur_time)
VALUES (NULL, NULLIF(@@tidb_current_ts, 0), ?, ?, ?, NOW(6), ?)`, c.prefix+baseMessagesTable)}
}

var _ outbox.Store = (*Store)(nil)

// Compile-time capability pins for *RelayStore: the sequence runtime discovers
// every optional capability by type assertion, so signature drift would
// otherwise downgrade silently (always-leader, no sequencer, no sweep).
var (
	_ sequence.Sequencer = (*RelayStore)(nil)
	_ sequence.Sweeper   = (*RelayStore)(nil)
	_ relay.LeaderStore  = (*RelayStore)(nil)
)

// RelayStore is the pool-backed relay store: everything the relay runtimes
// need — read/offset, the sequencer, the retention sweep, and leader
// election. These all manage their own transactions against the pool, so they
// require a *sql.DB rather than a transaction-scoped Runner.
//
// RelayStore deliberately does NOT implement outbox.Store: publishing
// requires a tx-scoped Store (NewStore over a *sql.Tx) so the outbox row
// commits atomically with business writes — on the autocommit pool
// CreateOutboxMessage always fails. Do not re-add a *Store embed here: the
// promoted method would make outbox.NewSender(relayStore) compile and then
// fail at the first publish.
type RelayStore struct {
	db *sql.DB
	q  relayQueries
}

// relayQueries holds every relay-side statement, rendered once at
// construction: the table names are identifiers and cannot be bound
// parameters, and WithTablePrefix validates the only variable fragment.
type relayQueries struct {
	list           string
	offset         string
	commitOffset   string
	initOffset     string
	deleteOffset   string
	probePending   string
	lockSequencer  string
	assignSeq      string
	bumpSequencer  string
	acquireLock    string
	readLockHolder string
	releaseLock    string
	sweep          string
}

func buildRelayQueries(c config) relayQueries {
	messages := c.prefix + baseMessagesTable
	offsets := c.prefix + baseOffsetsTable
	sequencers := c.prefix + baseSequencersTable
	locks := c.prefix + baseLocksTable
	return relayQueries{
		list: fmt.Sprintf(`
SELECT seq, event_id, metadata, data, create_time
FROM %s
WHERE seq > ?
ORDER BY seq
LIMIT ?`, messages),
		offset: fmt.Sprintf(`SELECT last_seq FROM %s WHERE name = ?`, offsets),
		commitOffset: fmt.Sprintf(`
INSERT INTO %s (name, last_seq, update_time)
VALUES (?, ?, NOW(6))
ON DUPLICATE KEY UPDATE
    last_seq    = GREATEST(last_seq, VALUES(last_seq)),
    update_time = VALUES(update_time)`, offsets),
		initOffset: fmt.Sprintf(`
INSERT INTO %s (name, last_seq, update_time)
SELECT ?, COALESCE(MAX(seq), 0), NOW(6) FROM %s`, offsets, messages),
		deleteOffset: fmt.Sprintf(`DELETE FROM %s WHERE name = ?`, offsets),
		probePending: fmt.Sprintf(`SELECT 1 FROM %s WHERE seq IS NULL LIMIT 1`, messages),
		lockSequencer: fmt.Sprintf(
			`SELECT next_seq FROM %s WHERE name = 'default' FOR UPDATE`, sequencers),
		// The page LIMIT sits in the innermost derived table, BELOW the window:
		// TiDB does not push Limit below Sort/Window, so a single-block
		// `ROW_NUMBER() OVER (...) ... LIMIT ?` ships the ENTIRE pending backlog
		// to the root executor and fully sorts it on every pass — O(backlog) per
		// pass, O(N²/batch) to recover from a long outage. With the inner LIMIT
		// the TopN is pushed into TiKV and only the page reaches the root, where
		// the window numbers just those rows.
		assignSeq: fmt.Sprintf(`
UPDATE %s o
JOIN (
    SELECT id, ROW_NUMBER() OVER (ORDER BY tx_start_ts, id) AS rn
    FROM (
        SELECT id, tx_start_ts FROM %s
        WHERE seq IS NULL
        ORDER BY tx_start_ts, id
        LIMIT ?
    ) page
) b ON b.id = o.id
SET o.seq = ? + b.rn - 1`, messages, messages),
		bumpSequencer: fmt.Sprintf(`UPDATE %s SET next_seq = ? WHERE name = 'default'`, sequencers),
		acquireLock: fmt.Sprintf(`
INSERT INTO %s (name, holder_id, expire_time)
VALUES (?, ?, NOW(6) + INTERVAL ? MICROSECOND)
ON DUPLICATE KEY UPDATE
    holder_id   = IF(expire_time < NOW(6) OR holder_id = VALUES(holder_id), VALUES(holder_id), holder_id),
    expire_time = IF(expire_time < NOW(6) OR holder_id = VALUES(holder_id), VALUES(expire_time), expire_time)`, locks),
		readLockHolder: fmt.Sprintf(`SELECT holder_id FROM %s WHERE name = ?`, locks),
		releaseLock:    fmt.Sprintf(`DELETE FROM %s WHERE name = ? AND holder_id = ?`, locks),
		// The cutoff is evaluated against the DATABASE clock (NOW(6)), per the
		// Sweeper contract: a skewed relay host must not sweep early or
		// pin rows forever.
		sweep: fmt.Sprintf(`
DELETE FROM %s
WHERE seq IS NOT NULL
  AND seq <= (SELECT MIN(last_seq) FROM %s)
  AND create_time < NOW(6) - INTERVAL ? MICROSECOND
LIMIT ?`, messages, offsets),
	}
}

// NewRelayStore builds a relay store over db. Use the same WithTablePrefix as
// the instance's publish Store (NewStore) — the two sides address one
// four-table outbox instance.
//
// DSN note: with go-sql-driver/mysql, parseTime=true is REQUIRED — ListMessages
// scans create_time (DATETIME(6)) into time.Time, and with the driver default
// (parseTime=false) every relay page fails with a []uint8→time.Time scan
// error. The failure only appears once the relay runs (publishing works fine),
// far from the misconfiguration. interpolateParams=true is additionally
// recommended (see NewStore).
func NewRelayStore(db *sql.DB, opts ...Option) *RelayStore {
	var c config
	for _, opt := range opts {
		opt(&c)
	}
	return &RelayStore{db: db, q: buildRelayQueries(c)}
}

var (
	_ sequence.Store     = (*RelayStore)(nil)
	_ sequence.Sequencer = (*RelayStore)(nil)
	_ sequence.Sweeper   = (*RelayStore)(nil)
	_ relay.LeaderStore  = (*RelayStore)(nil)
)

// CreateOutboxMessage inserts an unsequenced row. tx_start_ts is the publishing
// transaction's PD start TSO (@@tidb_current_ts); id is auto-assigned in insert
// order (= emit order); seq stays NULL until the sequencer runs. This MUST run
// inside a transaction (a transaction-scoped Runner): the row then commits
// atomically with business writes, and tx_start_ts is only available on
// transactional connections — an autocommit publish fails with a descriptive
// error.
//
// Requires Message.ID to be a UUID string: event_id is a BINARY(16) column, and
// m.ID is parsed as a UUID before the insert. A custom outbox.RowIDGenerator
// (via outbox.WithRowIDGenerator) MUST emit UUIDs to be usable with this store.
func (s *Store) CreateOutboxMessage(ctx context.Context, m *outbox.Message) error {
	if m.Metadata == nil {
		return errors.New("outbox: message metadata is nil")
	}
	id, err := uuid.Parse(m.ID)
	if err != nil {
		return fmt.Errorf("outbox: parse message ID %q: %w", m.ID, err)
	}
	md := m.Metadata
	if md.Time.IsZero() {
		// A zero time would be sent as 0001-01-01, below DATETIME(6)'s minimum,
		// and surface as an opaque driver error; reject it up front instead.
		return errors.New("outbox: message metadata time is zero; set Metadata.Time before publishing")
	}
	meta, err := json.Marshal(md)
	if err != nil {
		return fmt.Errorf("outbox: marshal metadata: %w", err)
	}
	// Message.CreateTime is deliberately IGNORED here: the row's create_time
	// is stamped by the database clock (see NewStore) because it anchors
	// retention and must not trust publisher clocks. ListMessages returns the
	// DB-stamped value.
	_, err = s.r.ExecContext(ctx, s.insertQuery,
		id[:], meta, m.Data, md.Time.UTC(),
	)
	if err != nil {
		if isNullColumn(err, "tx_start_ts") {
			return fmt.Errorf("outbox: CreateOutboxMessage must run inside a transaction (tx_start_ts is only available on transactional connections): %w", err)
		}
		return fmt.Errorf("outbox: insert: %w", err)
	}
	return nil
}

// ListMessages returns sequenced rows with seq > afterSeq in seq order. If a
// row's persisted metadata fails to decode, it stops at that row and returns
// the successfully decoded prefix together with a *sequence.DecodeError
// identifying the poison row (per the sequence.Store contract).
func (rs *RelayStore) ListMessages(ctx context.Context, afterSeq int64, limit int) ([]*outbox.Message, error) {
	rows, err := rs.db.QueryContext(ctx, rs.q.list, afterSeq, limit)
	if err != nil {
		return nil, fmt.Errorf("outbox: list: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := make([]*outbox.Message, 0, limit)
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
		// Unmarshal into a *pointer* target: JSON `null` unmarshals into a
		// struct value WITHOUT error (leaving it zero), and a zero-metadata
		// event would slip past the poison classification and be sent
		// downstream as an empty event. With a pointer target, `null` yields
		// nil and is classified poison like any other undecodable row.
		var md *event.Metadata
		if err := json.Unmarshal(meta, &md); err != nil {
			// Poison row: hand the decoded prefix back with a typed error so
			// the relay can deliver up to the poison row and then park it (or
			// stop the lane) instead of blocking on the whole page.
			return out, &sequence.DecodeError{ID: id.String(), Seq: seq, Err: err}
		}
		if md == nil {
			return out, &sequence.DecodeError{ID: id.String(), Seq: seq, Err: errors.New("persisted metadata is JSON null")}
		}
		if md.Time.IsZero() {
			// JSON-valid-but-empty metadata ("{}") decodes into a non-nil md
			// with every field zero. The write side rejects zero Metadata.Time
			// (see CreateOutboxMessage), so such a row was not written by this
			// store — classify it poison like the null case instead of sending
			// an empty event downstream.
			return out, &sequence.DecodeError{ID: id.String(), Seq: seq, Err: errors.New("persisted metadata has zero time")}
		}
		out = append(out, &outbox.Message{
			ID:         id.String(),
			Seq:        seq,
			Metadata:   md,
			Data:       data,
			CreateTime: createTime,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("outbox: list rows: %w", err)
	}
	return out, nil
}

// Offset returns the named consumer's watermark (0 if unset).
func (rs *RelayStore) Offset(ctx context.Context, name string) (int64, error) {
	var seq int64
	err := rs.db.QueryRowContext(ctx, rs.q.offset, name).Scan(&seq)
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
	_, err := rs.db.ExecContext(ctx, rs.q.commitOffset, name, seq)
	if err != nil {
		return fmt.Errorf("outbox: commit offset: %w", err)
	}
	return nil
}

// InitOffsetLatest creates the named consumer group's offset row at the
// current maximum assigned seq (0 if the log is empty or unsequenced) ONLY if
// no row exists yet, and returns the effective committed offset.
// Insert-if-absent: an existing row — even one at 0 — is a committed position
// and is never modified. Forward-jumping an existing row (the old GREATEST
// upsert) silently lost events: a group primed on an empty log commits a row
// at 0, and a re-init after a relay restart jumped it 0 → MAX(seq), skipping
// everything pending.
//
// The row-exists case is detected via the duplicate-key error rather than
// INSERT IGNORE: IGNORE downgrades every ignorable error to a warning — a
// too-long name would be silently truncated into a different group's row —
// while tolerating exactly ER_DUP_ENTRY keeps all other failures loud. The
// duplicate is expected, not an anomaly: it fires on each relay restart while
// the group still sits at offset 0, and in the split-brain race where two
// leaders init concurrently; the read-back below returns the surviving row
// either way.
func (rs *RelayStore) InitOffsetLatest(ctx context.Context, name string) (int64, error) {
	_, err := rs.db.ExecContext(ctx, rs.q.initOffset, name)
	if err != nil && !isDuplicateKey(err) {
		return 0, fmt.Errorf("outbox: init offset latest: %w", err)
	}
	var seq int64
	if err := rs.db.QueryRowContext(ctx, rs.q.offset, name).Scan(&seq); err != nil {
		return 0, fmt.Errorf("outbox: init offset latest read back: %w", err)
	}
	return seq, nil
}

// isDuplicateKey reports whether err is MySQL/TiDB ER_DUP_ENTRY (1062).
func isDuplicateKey(err error) bool {
	me, ok := errors.AsType[*mysql.MySQLError](err)
	return ok && me.Number == 1062
}

// isNullColumn reports whether err is MySQL/TiDB ER_BAD_NULL_ERROR (1048,
// "Column '<col>' cannot be null") on the named column. The number alone is
// not enough — any NOT NULL column raises 1048 (e.g. a nil Data on the data
// column), so the message is matched for the specific column too.
func isNullColumn(err error, col string) bool { //nolint:unparam // kept generic: 1048 fires for ANY NOT NULL column; the argument names which guard this is
	me, ok := errors.AsType[*mysql.MySQLError](err)
	return ok && me.Number == 1048 && strings.Contains(me.Message, col)
}

// DeleteOffset removes the named consumer group's offset row. It is the
// decommissioning step for a retired consumer group: a retired group's stale
// offset row keeps pinning MIN(last_seq) (see SweepMessages) and halts
// retention permanently until the row is deleted. Deleting a missing row is a
// no-op.
func (rs *RelayStore) DeleteOffset(ctx context.Context, name string) error {
	if _, err := rs.db.ExecContext(ctx, rs.q.deleteOffset, name); err != nil {
		return fmt.Errorf("outbox: delete offset: %w", err)
	}
	return nil
}

// SequenceMessages assigns dense seq values to committed pending rows in
// (tx_start_ts, id) order. The counter row is locked FOR UPDATE for the whole
// pass, so concurrent sequencers serialize and can never double-assign.
func (rs *RelayStore) SequenceMessages(ctx context.Context, limit int) (int, error) {
	// Idle fast path: probe for pending work on the pool before opening the
	// sequencing transaction. An idle tick otherwise pays BEGIN + FOR UPDATE +
	// UPDATE + COMMIT (~4 round trips) and a pessimistic lock on the counter
	// row every PollInterval per relay. Racing a freshly committed row merely
	// defers it one tick — the same cadence the poll already imposes.
	var one int
	err := rs.db.QueryRowContext(ctx, rs.q.probePending).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("outbox: probe pending: %w", err)
	}

	tx, err := rs.db.BeginTx(ctx, &sql.TxOptions{})
	if err != nil {
		return 0, fmt.Errorf("outbox: begin sequence tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // no-op after Commit

	var next int64
	if err := tx.QueryRowContext(ctx, rs.q.lockSequencer).Scan(&next); err != nil {
		return 0, fmt.Errorf("outbox: lock sequencer: %w", err)
	}

	res, err := tx.ExecContext(ctx, rs.q.assignSeq, limit, next)
	if err != nil {
		return 0, fmt.Errorf("outbox: assign seq: %w", err)
	}
	assigned, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("outbox: rows affected: %w", err)
	}

	if assigned > 0 {
		if _, err := tx.ExecContext(ctx, rs.q.bumpSequencer, next+assigned); err != nil {
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
	if _, err := rs.db.ExecContext(ctx, rs.q.acquireLock,
		name, holderID, ttl.Microseconds(),
	); err != nil {
		return false, fmt.Errorf("outbox: acquire lock: %w", err)
	}

	var holder string
	err := rs.db.QueryRowContext(ctx, rs.q.readLockHolder, name).Scan(&holder)
	if errors.Is(err, sql.ErrNoRows) {
		// The upsert and the read-back are two non-transactional statements: a
		// live holder's graceful ReleaseLeaderLock can delete the row between
		// them. No row simply means we don't hold the lock — the semantically
		// correct answer is false, not an error (which would cost the caller a
		// spurious pass failure); the next tick's acquire takes the free lock.
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("outbox: read lock holder: %w", err)
	}
	return holder == holderID, nil
}

// ReleaseLeaderLock drops the lock if still held by holderID.
func (rs *RelayStore) ReleaseLeaderLock(ctx context.Context, name, holderID string) error {
	_, err := rs.db.ExecContext(ctx, rs.q.releaseLock, name, holderID)
	if err != nil {
		return fmt.Errorf("outbox: release lock: %w", err)
	}
	return nil
}

// SweepMessages deletes sequenced rows at or below the minimum committed offset
// across all consumers and inserted (create_time) more than `olderThan` ago —
// per the DATABASE clock on both sides: create_time is DB-stamped at insert
// (see CreateOutboxMessage) and the cutoff is NOW(6)-relative, so no
// publisher or relay host clock can sweep early or pin rows forever.
// Retention is anchored to insert time, not event time, so a backdated
// WithEventTime event is not swept early. If no offsets exist yet,
// MIN(last_seq) is NULL and nothing is deleted.
//
// MIN(last_seq) spans only registered offset rows: a consumer group that was
// created but never run (or InitOffsetLatest'd) has no outbox_offsets row and
// so provides no retention protection at all — an unrun group does not hold
// the sweep back. Consumer groups must run (or call InitOffsetLatest) within
// the retention window to be protected from the sweep.
//
// Conversely, a RETIRED group's offset row stops advancing but keeps pinning
// MIN(last_seq) at its last committed position, halting the sweep permanently.
// Decommission a retired consumer group with DeleteOffset to unpin retention.
func (rs *RelayStore) SweepMessages(ctx context.Context, olderThan time.Duration, limit int) (int, error) {
	res, err := rs.db.ExecContext(ctx, rs.q.sweep, olderThan.Microseconds(), limit)
	if err != nil {
		return 0, fmt.Errorf("outbox: sweep: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("outbox: sweep rows affected: %w", err)
	}
	return int(n), nil
}
