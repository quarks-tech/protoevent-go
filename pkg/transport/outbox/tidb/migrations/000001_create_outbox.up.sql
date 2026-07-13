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
-- index widened to (seq, tx_start_ts, emit_ord). This preserves the ordering
-- contract exactly: cross-transaction order comes from tx_start_ts, NEXTVAL is
-- monotonic per connection so intra-tx emit order holds, and sequence gaps are
-- irrelevant because density comes from seq (a pre-commit-allocated value is
-- only unsafe as the delivery watermark, not as a tiebreak among rows the
-- post-commit sequencer already sees). Costs, which is why it is not the
-- default: only the row-data hotspot dies (new idx_outbox_seq entries are
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
    KEY idx_outbox_seq (seq, tx_start_ts)
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
