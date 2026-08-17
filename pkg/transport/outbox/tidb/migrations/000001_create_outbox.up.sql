-- The complete outbox schema, as one migration.
--
-- It was briefly four (create, then three index changes) while the index set was
-- being worked out, and the intermediate states never shipped — this module has no
-- released tag. Collapsing them keeps a fresh install from building an index only to
-- drop it again, and keeps the schema's rationale in one place instead of spread
-- across a changelog of files nobody reads in order.
--
-- id is deliberately AUTO_INCREMENT, not the AUTO_RANDOM default (ADR 001/012
-- exception for strictly-increasing IDs): the sequencer orders same-TSO rows by
-- id, so insert order must be monotonic. The resulting tail-Region write
-- hotspot is accepted and bounded by publish volume; event_id (a UUID) is what
-- scatters lookups. seq is NULL until the post-commit sequencer pass assigns
-- it (ADR 003 documented exception): rows are invisible to the relay until
-- sequenced, which is what closes the assigned-vs-visible commit gap.
-- UNSIGNED BIGINT values here always fit in the application's int64 (TSOs and
-- counters stay far below 2^63).
--
-- Escape hatch if a deployment actually shows Region imbalance from the insert
-- hotspot: switch id to AUTO_RANDOM and move the intra-transaction tiebreak to
-- a column fed by a TiDB SEQUENCE — `emit_ord BIGINT UNSIGNED NOT NULL` filled
-- with NEXTVAL(...) in the INSERT, sequencer ORDER BY (tx_start_ts, emit_ord),
-- idx_outbox_seq_order widened to (seq, tx_start_ts, emit_ord). This preserves the
-- ordering contract exactly: cross-transaction order comes from tx_start_ts, NEXTVAL is
-- monotonic per connection so intra-tx emit order holds, and sequence gaps are
-- irrelevant because density comes from seq (a pre-commit-allocated value is
-- only unsafe as the delivery watermark, not as a tiebreak among rows the
-- post-commit sequencer already sees). Costs, which is why it is not the
-- default: only the row-data hotspot dies (new idx_outbox_seq_order entries are
-- (NULL, monotonic-TSO, ...) and still append to one index tail), the drain's
-- reads by seq lose clustered-PK locality and become scattered point lookups,
-- and the SEQUENCE is one more schema object. Measured (2026-07, TiDB v7.5.1,
-- 200k rows x 1 KiB, dense seq, two interleaved full-log walks): the variant
-- drains ~24-25% slower and gains nothing on publish (-4%, NEXTVAL itself
-- ~7us) — and that was single-node, where the widened index alone accounts
-- for the gap and the PK-scatter cost (per-Region gRPC fan-out on the
-- table-side lookups) is not priced at all, so treat ~25% as a lower bound.
-- Prefer this over SHARD_ROW_ID_BITS for this table: sharding row ids alone
-- would randomize the sequencer's same-TSO tiebreak and silently break
-- intra-transaction ordering.
--
-- THE THREE SECONDARY INDEXES, and why each is irreducible:
--
-- uk_outbox_event (event_id) — idempotency / consumer dedup. Random UUID, so its
-- writes are scattered rather than appending.
--
-- uk_outbox_seq (seq) — the completeness invariant, and ONLY that. It is a strict
-- prefix of idx_outbox_seq_order below, so every read it could serve (including the
-- drain's `WHERE seq > ? ORDER BY seq`) is already served there; do not keep it for a
-- query's sake. Keep it because UNIQUE cannot be expressed on that wider index, whose
-- 3-tuple is trivially unique.
--   The Sequencer contract asserts that passes serialize on the counter row taken FOR
--   UPDATE and so "can never double-assign" — but that is a property of the SESSION,
--   not of the table, so without this constraint the invariant fails OPEN. And failing
--   open here is permanent, total, silent loss: a row assigned a seq already below
--   every consumer's committed offset is never returned by `WHERE seq > ?`, so it is
--   never delivered, and SweepMessages later deletes it as fully consumed. No error,
--   no OnError, no OnSwept anomaly. The counter and the data diverge more easily than
--   it sounds — restoring the small outbox_sequencers table from an older backup, or a
--   partially applied DDL change on a prefixed instance. With the constraint the
--   offending UPDATE aborts with ER_DUP_ENTRY (1062), which SequenceMessages already
--   surfaces as a pass error: loud, and recoverable by fixing the counter.
--   NULLs are exempt from UNIQUE in MySQL/TiDB, which is what makes the constraint
--   expressible at all on a column that is NULL until the post-commit pass assigns it.
--
-- idx_outbox_seq_order (seq, tx_start_ts, id) — the sequencer's page, which must cost
-- O(page) and not O(backlog). Its query is
-- `WHERE seq IS NULL ORDER BY seq, tx_start_ts, id LIMIT ?`, and two things are needed
-- to get index order rather than a TopN. NEITHER works alone:
--   1. id must be IN THE KEY. id is the CLUSTERED primary key, so a secondary index
--      stores it as the row handle, not as an ordered key suffix — which is why
--      (seq, tx_start_ts) cannot satisfy an ORDER BY ending in id.
--   2. The query's ORDER BY must LEAD with seq, this index's first column (see
--      assignSeq in store.go). Ordering by seq first cannot change the result order,
--      because the WHERE clause pins seq to a single value (NULL).
--   Measured on TiDB v7.5.1 at a 20,000-row pending backlog — with both:
--   `Limit -> IndexReader -> Limit -> IndexRangeScan  actRows 2048`; with either
--   missing: `TopN -> IndexReader -> TopN -> IndexRangeScan  actRows 20000`, i.e. one
--   pass reads the whole backlog and outage recovery becomes quadratic (a one-day
--   backlog at 115 events/s is 10M rows, 10,000 passes each scanning up to 10M index
--   entries) while holding the pessimistic sequencer lock that blocks every other
--   consumer group. Once one pass exceeds OpTimeout the sequencer stops progressing
--   at all and a recoverable backlog becomes a permanent stall.
--
-- idx_outbox_create_time (create_time) — the retention sweep's cutoff. The sweep
-- deletes rows below MIN(offset) AND older than the NOW(6)-relative cutoff; the seq
-- predicate matches most of the table in steady state, so without this index the
-- optimizer falls back to a full table scan on every terminal sweep pass (measured:
-- 83ms at 200k rows, growing linearly) — and when nothing is deletable, EVERY pass is
-- the terminal pass. With it the cutoff bounds the scan, making idle sweeps near-O(1).
CREATE TABLE outbox_messages (
    id           BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
    seq          BIGINT UNSIGNED NULL,
    tx_start_ts  BIGINT UNSIGNED NOT NULL,
    event_id     BINARY(16)      NOT NULL,
    metadata     JSON            NOT NULL,
    data         MEDIUMBLOB      NOT NULL,
    create_time  DATETIME(6)     NOT NULL,
    occur_time   DATETIME(6)     NOT NULL,
    PRIMARY KEY (id) /*T![clustered_index] CLUSTERED */,
    UNIQUE KEY uk_outbox_event (event_id),
    UNIQUE KEY uk_outbox_seq (seq),
    KEY idx_outbox_seq_order (seq, tx_start_ts, id),
    KEY idx_outbox_create_time (create_time)
) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin;

CREATE TABLE outbox_sequencers (
    name     VARCHAR(64)     NOT NULL,
    next_seq BIGINT UNSIGNED NOT NULL,
    PRIMARY KEY (name)
) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin;

INSERT INTO outbox_sequencers (name, next_seq) VALUES ('default', 1);

CREATE TABLE outbox_offsets (
    name        VARCHAR(64)     NOT NULL,
    last_seq    BIGINT UNSIGNED NOT NULL,
    update_time DATETIME(6)     NOT NULL,
    PRIMARY KEY (name)
) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin;

CREATE TABLE relay_locks (
    name        VARCHAR(64) NOT NULL,
    holder_id   VARCHAR(64) NOT NULL,
    expire_time DATETIME(6) NOT NULL,
    PRIMARY KEY (name)
) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin;
