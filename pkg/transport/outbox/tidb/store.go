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
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/google/uuid"

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

// defaultRetentionWindow is how much history the retention sweep keeps (see
// WithRetentionWindow). The MongoDB sibling store makes the same choice with
// its TTL index, so one outbox reads the same on either backend.
const defaultRetentionWindow = 7 * 24 * time.Hour

// config carries construction-time configuration shared by NewStore and
// NewRelayStore.
type config struct {
	prefix    string
	retention time.Duration // sweep cutoff age; see WithRetentionWindow
}

// newConfig applies opts over the defaults.
func newConfig(opts []Option) config {
	c := config{retention: defaultRetentionWindow}
	for _, opt := range opts {
		opt(&c)
	}
	return c
}

// Option configures a Store / RelayStore instance.
type Option func(*config)

// WithTablePrefix names this outbox instance: all four tables (and, by
// convention, the golang-migrate versions table — see PrefixedMigrations) get
// the prefix, letting several independent outboxes coexist in one schema,
// each with its own total order, retention, and consumer groups. The publish
// Store and the RelayStore of one instance MUST use the same prefix.
//
// The prefix is an SQL identifier fragment ([A-Za-z][A-Za-z0-9_]*, at most
// outbox.MaxInstancePrefixLen characters, e.g. "orders_") — the rule is shared
// with the other backends' instance prefixes via
// outbox.ValidateInstancePrefix, so one prefix value works verbatim on all of
// them. An invalid prefix panics: it is static developer configuration, not
// runtime input — the regexp.MustCompile convention for programmer error.
func WithTablePrefix(prefix string) Option {
	if err := outbox.ValidateInstancePrefix("table prefix", prefix); err != nil {
		panic(err)
	}
	return func(c *config) { c.prefix = prefix }
}

// WithRetentionWindow sets how long a delivered row survives before
// SweepMessages may delete it (default 7 days). The cutoff is evaluated
// against the DATABASE clock, never a relay host's — see SweepMessages.
//
// The window belongs to the STORE, not to a relay: the sweep's cutoff is
// MIN(last_seq) across ALL consumer groups, so its effect is store-wide while
// a relay is per-group. Configured per relay, the shortest window silently won
// for everybody — a second consumer group could truncate the first's 30-day
// history to a 7-day default, diagnosable only by comparing two startup logs.
// Every relay over this store therefore inherits one window, which is what
// retention on a shared log always was.
//
// Size it above the longest consumer downtime you intend to survive: a group
// that is down longer than the window loses the events swept out from under
// its offset. A non-positive window panics — static developer configuration,
// same convention as WithTablePrefix.
func WithRetentionWindow(d time.Duration) Option {
	if d <= 0 {
		panic(fmt.Errorf("outbox: retention window must be > 0, got %v", d))
	}
	return func(c *config) { c.retention = d }
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
	c := newConfig(opts)
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
	// retention is the sweep's cutoff age (WithRetentionWindow). It is bound
	// to the store rather than passed per SweepMessages call because the
	// sweep's effect is store-wide — see WithRetentionWindow.
	retention time.Duration
}

// relayQueries holds every relay-side statement, rendered once at
// construction: the table names are identifiers and cannot be bound
// parameters, and WithTablePrefix validates the only variable fragment.
type relayQueries struct {
	list          string
	offset        string
	commitOffset  string
	initOffset    string
	deleteOffset  string
	probePending  string
	lockSequencer string
	assignSeq     string
	bumpSequencer string
	acquireLock   string
	releaseLock   string
	sweep         string
	storeNow      string
}

// Leader-lock outcomes, as reported by acquireLock's LastInsertId.
//
//   - lockInserted (0) is not a value the statement writes: it is what the OK
//     packet carries when the INSERT succeeded outright, because ON DUPLICATE
//     KEY UPDATE never ran and relay_locks has no AUTO_INCREMENT column (see
//     migrations/000001_create_outbox.up.sql — a generated id there would
//     collide with the sentinels below). A fresh insert means the lock was free
//     and is now ours.
//   - lockTaken (1): the row existed and the conditional took or renewed it.
//   - lockLost (2): the row existed, belongs to a live holder, and was left
//     untouched.
//
// Any other value is treated as an error, not as leadership: this is the one
// place where guessing "probably ours" turns into two active leaders.
const (
	lockInserted = 0
	lockTaken    = 1
	lockLost     = 2
)

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
		// The upsert both decides the election AND reports its own outcome, so
		// TryAcquireLeaderLock is ONE statement per tick rather than an upsert
		// plus a read-back (which was two round trips per relay per
		// PollInterval, forever, even on an idle relay).
		//
		// The outcome is carried by LAST_INSERT_ID(expr), which sets the value
		// the OK packet returns as LastInsertId, and NOT by affected-rows.
		// Affected-rows looks like it would do (1 inserted / 2 updated / 0
		// unchanged) and is wrong: with CLIENT_FOUND_ROWS in the DSN
		// (go-sql-driver's clientFoundRows=true) an unchanged row reports 1
		// instead of 0, making a LOST election indistinguishable from a won
		// insert — every replica would believe it holds the lock. Verified on
		// TiDB v7.5.1 in both DSN modes; LAST_INSERT_ID is unaffected by the
		// flag. See lockInserted/lockTaken/lockLost for the encoding.
		//
		// The second assignment deliberately re-tests the SAME condition rather
		// than reusing the first: MySQL evaluates ON DUPLICATE KEY UPDATE
		// assignments left to right, so `holder_id` here is already the NEW
		// value — which makes `holder_id = VALUES(holder_id)` true exactly when
		// the first IF took the lock, and false when it left the incumbent
		// alone. expire_time is still the OLD value at this point, so the
		// expiry branch reads the incumbent's deadline as intended.
		acquireLock: fmt.Sprintf(`
INSERT INTO %s (name, holder_id, expire_time)
VALUES (?, ?, NOW(6) + INTERVAL ? MICROSECOND)
ON DUPLICATE KEY UPDATE
    holder_id   = IF(expire_time < NOW(6) OR holder_id = VALUES(holder_id), VALUES(holder_id), holder_id),
    expire_time = IF(LAST_INSERT_ID(IF(expire_time < NOW(6) OR holder_id = VALUES(holder_id), %d, %d)) = %d,
                     VALUES(expire_time), expire_time)`, locks, lockTaken, lockLost, lockTaken),
		releaseLock: fmt.Sprintf(`DELETE FROM %s WHERE name = ? AND holder_id = ?`, locks),
		// The cutoff is evaluated against the DATABASE clock (NOW(6)), per the
		// Sweeper contract: a skewed relay host must not sweep early or
		// pin rows forever. The bound parameter is the store's own retention
		// window (WithRetentionWindow), not a per-call one.
		sweep: fmt.Sprintf(`
DELETE FROM %s
WHERE seq IS NOT NULL
  AND seq <= (SELECT MIN(last_seq) FROM %s)
  AND create_time < NOW(6) - INTERVAL ? MICROSECOND
LIMIT ?`, messages, offsets),
		storeNow: `SELECT NOW(6)`,
	}
}

// NewRelayStore builds a relay store over db. Use the same WithTablePrefix as
// the instance's publish Store (NewStore) — the two sides address one
// four-table outbox instance. WithRetentionWindow tunes how much history the
// sweep keeps (default 7 days); the sweep's CADENCE is the relay's
// (sequence.WithRetention / sequence.WithoutRetention).
//
// DSN note: with go-sql-driver/mysql, parseTime=true is REQUIRED — ListMessages
// scans create_time (DATETIME(6)) into time.Time, and with the driver default
// (parseTime=false) every relay page fails with a []uint8→time.Time scan
// error. The failure only appears once the relay runs (publishing works fine),
// far from the misconfiguration. interpolateParams=true is additionally
// recommended (see NewStore).
func NewRelayStore(db *sql.DB, opts ...Option) *RelayStore {
	c := newConfig(opts)
	return &RelayStore{db: db, q: buildRelayQueries(c), retention: c.retention}
}

// Compile-time capability pins for *RelayStore: the sequence runtime discovers
// every optional capability by type assertion, so signature drift would
// otherwise downgrade silently (always-leader, no sequencer, no sweep).
var (
	_ sequence.Store     = (*RelayStore)(nil)
	_ sequence.Sequencer = (*RelayStore)(nil)
	_ sequence.Sweeper   = (*RelayStore)(nil)
	_ sequence.Clock     = (*RelayStore)(nil)
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
	// The shared write-side envelope rules (metadata present, Metadata.Time
	// non-zero) live in the parent module so both backends accept and reject the
	// same messages. The zero-time half matters HERE in particular: a zero time
	// would be sent as 0001-01-01, below DATETIME(6)'s minimum, and surface as an
	// opaque driver error from inside the caller's business transaction.
	if err := outbox.ValidateMetadata(m.Metadata); err != nil {
		return err
	}
	// The error names the remedy, because of WHERE this fails: the insert runs
	// inside the caller's business transaction, so a non-UUID id does not merely
	// fail the publish — it rolls back the user's whole request. The default row-ID
	// generator reuses Metadata.ID, and CloudEvents allows any unique string there,
	// so a service that publishes ids like "order-42-created" hits this on its first
	// write with no hint that the fix is one option away.
	id, err := uuid.Parse(m.ID)
	if err != nil {
		return fmt.Errorf("outbox: message ID %q is not a UUID (the event_id column is BINARY(16)): %w"+
			" — either publish UUID event ids, or decouple the row key from them with"+
			" outbox.NewSender(store, outbox.WithRowIDGenerator(outbox.GenerateUUIDv4))",
			m.ID, err)
	}
	md := m.Metadata
	meta, err := outbox.MarshalMetadata(md)
	if err != nil {
		return err
	}
	// A nil payload is an EMPTY payload, not a missing one. database/sql sends a
	// nil []byte as SQL NULL, and data is `MEDIUMBLOB NOT NULL`, so the insert
	// would fail with MySQL 1048 naming the column — from inside the caller's
	// business transaction, rolling back the whole request over an error that
	// describes the schema rather than the cause. And the input is ordinary:
	// proto.Marshal returns (nil, nil) for a typed-nil or empty message, so
	// publishing an event with no fields set lands here. The sibling MongoDB store
	// accepts nil, so normalizing is also what keeps the two backends agreeing.
	data := m.Data
	if data == nil {
		data = []byte{}
	}
	// Message.CreateTime is deliberately IGNORED here: the row's create_time
	// is stamped by the database clock (see NewStore) because it anchors
	// retention and must not trust publisher clocks. ListMessages returns the
	// DB-stamped value.
	_, err = s.r.ExecContext(ctx, s.insertQuery,
		id[:], meta, data, md.Time.UTC(),
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

	// One backing array for the whole page, not one heap object per row: a
	// default 100-row page used to cost 101 allocations (a full-size pointer
	// slice plus a &Message per row) and now costs three.
	//
	// The initial capacity is deliberately well BELOW limit. An idle relay
	// polls once a second and reads nothing, so the empty page is the common
	// case and must not pay for limit messages; the first row that overflows
	// grows the buffer straight to limit, which the SQL LIMIT guarantees is
	// enough. Letting append double instead would allocate past the page size
	// (16, 32, 64, 128 for limit=100) and leave more garbage than the
	// per-row allocations it replaced — measured 9% MORE bytes per page.
	//
	// Pointers into buf are taken only after the last append (pagePointers),
	// so a growth reallocation cannot leave earlier entries aliasing a stale
	// array.
	buf := make([]outbox.Message, 0, min(limit, initialPageCap))
	for rows.Next() {
		if len(buf) == cap(buf) && cap(buf) < limit {
			grown := make([]outbox.Message, len(buf), limit)
			copy(grown, buf)
			buf = grown
		}
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
		// outbox.UnmarshalMetadata carries the three shared poison rules, which
		// exist because each one is a way an unusable row could otherwise reach
		// a consumer as if it were a real event: it decodes into a POINTER
		// target (JSON `null` unmarshals into a struct value WITHOUT error,
		// leaving it zero, so a null row would be sent downstream as an empty
		// event), classifies a nil result as poison, and classifies a zero
		// Metadata.Time as poison (JSON-valid-but-empty metadata "{}" decodes
		// non-nil with every field zero; the write side rejects a zero time —
		// see CreateOutboxMessage — so such a row was not written by this
		// library). They live in the parent module so the two backends cannot
		// disagree about whether the same row is deliverable.
		md, err := outbox.UnmarshalMetadata(meta)
		if err != nil {
			// Poison row: hand the decoded prefix back with a typed error so
			// the relay can deliver up to the poison row and then park it (or
			// stop the lane) instead of blocking on the whole page.
			return pagePointers(buf), &sequence.DecodeError{ID: id.String(), Seq: seq, Err: err}
		}
		buf = append(buf, outbox.Message{
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
	return pagePointers(buf), nil
}

// initialPageCap is the starting capacity of a ListMessages page — see there
// for why it is not the caller's limit.
const initialPageCap = 16

// pagePointers views a page's backing array as the []*outbox.Message the Store
// contract returns, without copying a message. An empty page returns nil: the
// relay only ever ranges over the result, and an idle poll should allocate
// nothing at all.
func pagePointers(msgs []outbox.Message) []*outbox.Message {
	if len(msgs) == 0 {
		return nil
	}
	out := make([]*outbox.Message, len(msgs))
	for i := range msgs {
		out[i] = &msgs[i]
	}
	return out
}

// Offset returns the named consumer's watermark (0 if unset).
func (rs *RelayStore) Offset(ctx context.Context, name string) (int64, bool, error) {
	var seq int64
	err := rs.db.QueryRowContext(ctx, rs.q.offset, name).Scan(&seq)
	if errors.Is(err, sql.ErrNoRows) {
		// No row: the relay primes. Distinct from a row holding 0, which is a
		// committed position and must not be re-primed — see sequence.Store.Offset.
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("outbox: offset: %w", err)
	}
	return seq, true, nil
}

// CommitOffset advances the watermark monotonically (GREATEST) and creates the
// row if absent — the relay registers a replay-from-beginning group by
// committing seq 0, so that the sweep's MIN(last_seq) cutoff accounts for it
// before it has delivered anything.
func (rs *RelayStore) CommitOffset(ctx context.Context, name string, seq int64) error {
	_, err := rs.db.ExecContext(ctx, rs.q.commitOffset, name, seq)
	if err != nil {
		return fmt.Errorf("outbox: commit offset: %w", err)
	}
	return nil
}

// StoreNow returns the database's current time (sequence.Clock), so the relay
// reports lag against the same clock that stamped create_time instead of the
// relay host's — see sequence.Clock for why that matters. NOW(6) matches the
// column's microsecond precision.
func (rs *RelayStore) StoreNow(ctx context.Context) (time.Time, error) {
	var now time.Time
	if err := rs.db.QueryRowContext(ctx, rs.q.storeNow).Scan(&now); err != nil {
		return time.Time{}, fmt.Errorf("outbox: store now: %w", err)
	}
	return now, nil
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
//
// One statement, no read-back: the upsert reports its own outcome through
// LastInsertId (see acquireLock and the lock* constants). Beyond saving a round
// trip per tick this removes a race the read-back had — between the upsert and
// the SELECT, a graceful ReleaseLeaderLock could delete the row we had just
// won, and we reported false for a lock we held.
func (rs *RelayStore) TryAcquireLeaderLock(ctx context.Context, name, holderID string, ttl time.Duration) (bool, error) {
	res, err := rs.db.ExecContext(ctx, rs.q.acquireLock, name, holderID, ttl.Microseconds())
	if err != nil {
		return false, fmt.Errorf("outbox: acquire lock: %w", err)
	}
	outcome, err := res.LastInsertId()
	if err != nil {
		return false, fmt.Errorf("outbox: acquire lock outcome: %w", err)
	}
	switch outcome {
	case lockInserted, lockTaken:
		return true, nil
	case lockLost:
		return false, nil
	default:
		// Fail closed and loudly. An unrecognized value means the statement no
		// longer means what the constants say (a schema with an AUTO_INCREMENT
		// on relay_locks, a driver that does not surface LAST_INSERT_ID), and
		// the safe reading of "I don't know" is "not the leader" — guessing the
		// other way runs every replica as leader.
		return false, fmt.Errorf("outbox: acquire lock: unexpected outcome %d from the leader-lock upsert "+
			"(want %d inserted, %d taken, or %d lost)", outcome, lockInserted, lockTaken, lockLost)
	}
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
// across all consumers and inserted (create_time) longer ago than this store's
// retention window (WithRetentionWindow, default 7 days) — per the DATABASE
// clock on both sides: create_time is DB-stamped at insert (see
// CreateOutboxMessage) and the cutoff is NOW(6)-relative, so no publisher or
// relay host clock can sweep early or pin rows forever.
// Retention is anchored to insert time, not event time, so a backdated
// WithEventTime event is not swept early. If no offsets exist yet,
// MIN(last_seq) is NULL and nothing is deleted.
//
// The window is the STORE's and not a parameter of this call because the
// cutoff is MIN(last_seq) over ALL consumer groups: the effect is store-wide,
// so a per-relay window would let the shortest one win for everybody. limit
// bounds one pass; the relay owns the cadence (sequence.WithRetention).
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
func (rs *RelayStore) SweepMessages(ctx context.Context, limit int) (int, error) {
	res, err := rs.db.ExecContext(ctx, rs.q.sweep, rs.retention.Microseconds(), limit)
	if err != nil {
		return 0, fmt.Errorf("outbox: sweep: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("outbox: sweep rows affected: %w", err)
	}
	return int(n), nil
}
