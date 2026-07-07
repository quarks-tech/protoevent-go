# Design: Sequenced-Log Outbox Relay

Status: DRAFT
Date: 2026-07-07
Owner: @filenko
Supersedes: two-table (`outbox_pending` / `outbox_completed`) relay model in `pkg/transport/outbox/relay`
Companion: `docs/design/outbox-mongodb-changestream.md` (MongoDB change-stream relay)

> **Package restructure (2026-07-07):** the MongoDB companion introduces a second runtime, so the
> shared primitives move to a common `relay` package and this (TiDB) runtime becomes `relay/sequence`:
> `relay` (shared: `Observer`, `Logger`, `LeaderStore`, `LeaderElector`, error policy) +
> `relay/sequence` (this doc: `sequence.Relay`, `Store`, `SequencerStore`, `RetentionStore`) +
> `relay/stream` (Mongo). Where this doc and the implementation plan say `relay.Relay` / `relay.Store`,
> read `sequence.Relay` / `sequence.Store`; the shared types live in `relay`. See the companion §3.

## 1. Problem

The current outbox model (`pkg/transport/outbox` + `relay`, implemented by chassis-go golden
templates) uses three tables:

- `outbox_pending` — queue drained by the relay (`ListPendingMessages` FIFO by `(create_time, id)`)
- `outbox_completed` — verbatim copy of the pending row + `sent_time`, written on complete
- `relay_lock` — leader election

Defects:

1. **`outbox_completed` is redundant.** It duplicates every column of the pending row for an
   "audit trail" the broker already provides. Costs: a second write per message, a second index,
   and (on MySQL, no TTL) a manual sweep loop (`SweepCompletedMessages`).
2. **Destructive drain.** Copy-then-delete means exactly one logical consumer. No replay, no
   second relay (e.g. broker-publish + search-indexer) tailing the same outbox.
3. **Ordering by `(create_time, id)` is unsound.** `create_time` is stamped at INSERT
   (app clock, per-node skew), not at COMMIT. It happens to be masked today because the relay
   re-reads the pending table from the beginning — but that property is exactly what couples the
   design to destructive drain (defect 2). Any cursor-based reader over this schema inherits the
   classic gap: a transaction that obtained an earlier ordering key but committed later is
   permanently skipped by a `key > cursor` reader.
4. **Copy → delete is two non-atomic statements.** Crash between them relies on `INSERT IGNORE`
   dedup; benign, but more machinery for the redundant table.

## 2. Goals

- Single append-only log table; no data-copying second table.
- Non-destructive consumption: N independent consumers ("relay names" = consumer groups), each
  with its own cursor, replay possible within a retention window.
- **Exact cursor** — no heuristic re-scan windows (backStep), no missed events, by construction.
- Ordering guarantee equivalent to one Kafka partition: total order per log, causal order
  preserved, at-least-once delivery with idempotent consumers.
- Target store is TiDB (relies on `@@tidb_current_ts` and clustered auto-increment PK); the
  `relay.Store` seam keeps the engine driver-agnostic, but no MySQL implementation is planned.
- Publish path stays contention-free (no hot row in the business transaction).

Non-goals:

- Exactly-once delivery (consumers dedup on `event_id`, as today).
- Ordering between genuinely concurrent transactions (no system provides this meaningfully;
  Kafka does not across concurrent producers either).
- Partitioned parallelism (designed to be layered later; see §9).

## 3. Core idea: post-commit sequencing

The gap problem exists because the ordering key is assigned *inside* the publishing transaction
(pre-commit) while visibility happens at COMMIT. Kafka avoids it by making assignment and
visibility a single atomic act performed by a single writer (the partition leader appends and
assigns the offset in one step).

We reproduce that in SQL: outbox rows are inserted with `seq = NULL`; a single leader-elected
**sequencer** assigns `seq` *after* commit, from a counter, seeing only committed rows.

- Sequencer is a single writer ⇒ `seq` is dense, gapless, monotone.
- A late-committing transaction's row is invisible to the sequencer until commit ⇒ it receives a
  *later* seq instead of hiding below the watermark ⇒ `seq > last_seq` cursors are exact.
- The counter row is locked `FOR UPDATE` for the duration of a sequencer pass ⇒ even overlapping
  leaders during lease failover cannot double-assign.

## 4. Schema

```sql
CREATE TABLE outbox (
    id           BIGINT       NOT NULL AUTO_INCREMENT,  -- physical PK only; NEVER used for ordering
                                                        -- (TiDB allocates id ranges per node → not time-monotone)
    seq          BIGINT       NULL,                     -- logical offset; NULL until sequenced post-commit
    tx_start_ts  BIGINT       NOT NULL,                 -- @@tidb_current_ts of publishing tx (PD start TSO)
    event_id     BINARY(16)   NOT NULL,
    `type`       VARCHAR(255) NOT NULL,
    source       VARCHAR(255) NOT NULL,
    subject      VARCHAR(255) NOT NULL,
    content_type VARCHAR(64)  NOT NULL,
    data         BLOB         NOT NULL,
    occurred_at  DATETIME(6)  NOT NULL,
    PRIMARY KEY (id) /*T![clustered_index] CLUSTERED */,
    UNIQUE KEY uk_outbox_event (event_id),
    -- one index serves both loops (id, the clustered PK, is implicitly appended by TiDB, so it
    -- is the within-tx tiebreak for the sequencer scan without being named here):
    --   sequencer: seq IS NULL → ordered by (tx_start_ts, id)
    --   drainer:   seq > ?     → ordered by seq
    KEY idx_outbox_seq (seq, tx_start_ts)
);

CREATE TABLE outbox_sequencer (
    name     VARCHAR(64) NOT NULL,   -- 'default'; one row per partition if partitioning is added later
    next_seq BIGINT      NOT NULL,
    PRIMARY KEY (name)
);

CREATE TABLE outbox_offsets (
    name        VARCHAR(64) NOT NULL,   -- consumer group (relay name)
    last_seq    BIGINT      NOT NULL,
    update_time DATETIME(6) NOT NULL,
    PRIMARY KEY (name)
);

CREATE TABLE relay_lock (                -- unchanged from current design
    name        VARCHAR(64) NOT NULL,
    holder_id   VARCHAR(64) NOT NULL,
    expire_time DATETIME(6) NOT NULL,
    PRIMARY KEY (name)
);
```

`outbox_offsets` and `outbox_sequencer` are cursor/counter rows, not data copies — they are the
consumer-group offset store and the partition-leader counter, in Kafka terms.

**Indexing.** Three indexes, and only one secondary index beyond the PK:

- `PRIMARY KEY (id)` clustered — serves publish inserts; `id` is also the implicit within-tx
  ordering tiebreak (appended to every secondary index, so it never needs naming in one).
- `UNIQUE KEY uk_outbox_event (event_id)` — idempotency / consumer dedup; random UUID, scattered.
- `KEY idx_outbox_seq (seq, tx_start_ts)` — serves **both** loops from one index: the drain range
  scan (`seq > @last ORDER BY seq`) uses `seq` as the leading column; the sequencer scan
  (`seq IS NULL ORDER BY tx_start_ts, id`) reads the NULL-prefix (NULLs sort first) already
  ordered by `tx_start_ts` then the appended `id`. No filesort on either path.

No `UNIQUE(seq)`: seq uniqueness/density is guaranteed by the counter-`FOR UPDATE` serialization
(§5.2), so a unique index would never fire in correct operation — it would only add a second
monotonic-write hotspot. The invariant is covered by tests and by `WithObserver` gap-detection
on the drain page (§10) instead.

**Hotspot note (TiDB).** `id` (monotonic clustered PK) and `idx_outbox_seq` (leading `seq`,
monotonic) both take writes at the high end → append hotspot on the last Region; `idx_outbox_seq`
is hot at two ends (publish inserts in the NULL-prefix, sequencing `UPDATE`s moving entries to the
high-`seq` end). This is inherent to a log table and matches the precedent (markerry txoutbox uses
a monotonic `seq` clustered PK). Acceptable up to ~one Region's write ceiling; TiDB auto-splits hot
Regions. If publish throughput saturates a Region, reach for `SHARD_ROW_ID_BITS` or partition lanes
(§9) — **not** `AUTO_RANDOM` on `id`, which would destroy the within-tx monotonicity the tiebreak
relies on.

## 5. SQL lifecycle

### 5.1 Publish — any app node, inside the business transaction

```sql
BEGIN;
UPDATE accounts SET ... WHERE id = ?;                       -- business write

SET @ts = @@tidb_current_ts;                                 -- this tx's start TSO
INSERT INTO outbox (seq, tx_start_ts, event_id, `type`, source, subject, content_type, data, occurred_at)
VALUES (NULL, @ts, ?, ?, ?, ?, ?, ?, ?);                     -- id auto-assigned in insert order = emit order
COMMIT;
```

No hot row is touched: publishers contend neither with each other nor with the relay.

### 5.2 Sequencer pass — leader only, one small transaction per pass

```sql
BEGIN;
-- Serialization point. Two nodes both believing they are leader during lease
-- failover serialize here → double assignment impossible.
SELECT next_seq FROM outbox_sequencer WHERE name = 'default' FOR UPDATE;   -- → @next

-- Snapshot sees COMMITTED rows only. An in-flight tx's row is invisible →
-- sequenced next pass → higher seq. ORDER BY before LIMIT: a causally-earlier
-- row is never cut from the batch while a later one is taken.
UPDATE outbox o
JOIN (
    SELECT id, ROW_NUMBER() OVER (ORDER BY tx_start_ts, id) AS rn
    FROM outbox
    WHERE seq IS NULL
    ORDER BY tx_start_ts, id
    LIMIT 500
) batch ON batch.id = o.id
SET o.seq = @next + batch.rn - 1;                                          -- ROW_COUNT() → @assigned

UPDATE outbox_sequencer SET next_seq = @next + @assigned WHERE name = 'default';
COMMIT;   -- assignment + counter atomic → seq dense & gapless; crash = clean rollback, redo next pass
```

### 5.3 Drain pass — leader, one per consumer group (independent cursors)

```sql
SELECT last_seq FROM outbox_offsets WHERE name = ?;                        -- → @last (0 if absent)

SELECT seq, event_id, `type`, source, subject, content_type, data, occurred_at
FROM outbox
WHERE seq > @last                                                          -- NULL seq excluded by > automatically
ORDER BY seq
LIMIT 1000;
-- seq is dense → cursor is EXACT: nothing can ever appear at ≤ @last later.

-- after the sender/handler succeeds for the page:
INSERT INTO outbox_offsets (name, last_seq, update_time)
VALUES (?, @max_seq_of_page, NOW(6))
ON DUPLICATE KEY UPDATE
    last_seq    = GREATEST(last_seq, VALUES(last_seq)),                    -- watermark never rewinds
    update_time = VALUES(update_time);
```

Handler or offset-commit failure leaves the offset unmoved → the page is redelivered →
at-least-once; consumers dedup on `event_id`.

### 5.4 Retention sweep — leader, periodic

```sql
DELETE FROM outbox
WHERE seq IS NOT NULL
  AND seq <= (SELECT MIN(last_seq) FROM outbox_offsets)                    -- every consumer passed it
  AND occurred_at < NOW(6) - INTERVAL 7 DAY                                -- replay window
LIMIT 5000;
```

Replaces the completed-table sweep: deletes only rows every consumer has passed, after a
retention window that permits replay.

## 6. Ordering guarantee

Three layers:

1. **Log construction.** Single sequencer, counter-locked ⇒ `seq` total order, dense,
   equal for every consumer.
2. **Within-batch rule** `ORDER BY tx_start_ts, id` decides causal order:
   - `tx_start_ts = @@tidb_current_ts` — PD-issued start TSO: constant across a transaction,
     globally monotone, and distinct per transaction (PD never hands two txs the same value).
     So `tx_start_ts` alone totally orders *across* transactions.
   - `id` (auto-increment PK) is the *within-transaction* tiebreak only. Inside one tx — one
     session, one node — auto-increment is assigned in insert order and is strictly monotone
     (`AUTO_ID_CACHE` introduces gaps and cross-node reordering, never intra-session reordering),
     and insert order = emit order because the eventbus publisher calls `Send` synchronously per
     `Publish`. The cross-tx ban on `id` (per-node range allocation → not time-monotone) still
     holds: the sequencer never compares `id` across transactions, because distinct `tx_start_ts`
     always separates them first.
3. **Consumption.** One leader per relay name, `ORDER BY seq`, sequential handling, offset
   committed only after success, `GREATEST` guard — order preserved under retry, never
   skip-ahead past a failure.

**Theorem.** If event B is causally after event A (B's transaction started after A's committed —
the read-modify-write case), then `seq(A) < seq(B)`.

*Sketch.* If A is sequenced in an earlier pass, done. If both are in one batch:
`tx_start_ts(B) > commit_ts(A) > tx_start_ts(A)` (TSO monotone; commit_ts > start_ts), so A
sorts first. The `ORDER BY ... LIMIT` batch cut cannot take B while dropping A. ∎

Not guaranteed: relative order of genuinely concurrent transactions (overlapping lifetimes, no
data dependency) — same semantics as one Kafka partition with concurrent producers. Transactions
that matter to each other (same aggregate) conflict on row locks, serialize, and fall under the
theorem.

### Race matrix

| race | outcome |
|---|---|
| tx gets low `tx_start_ts`, commits late | invisible until commit → later pass → **higher** seq; gap structurally impossible |
| causal pair A→B in one batch | tiebreak orders A first (theorem) |
| two leaders during lease failover | sequencer serializes on counter `FOR UPDATE`; duplicate drain = duplicate delivery, absorbed by idempotency + `GREATEST` |
| sequencer crash mid-pass | rollback — rows stay NULL, counter unmoved, no gaps |
| drainer crash after send, before offset commit | page redelivered — at-least-once by contract |
| publisher crash before COMMIT | row never visible; no ghost events |

## 7. protoevent-go changes

Module: `pkg/transport/outbox` (separate go.mod). **Clean major bump (v2)** — the legacy
two-table `relay.Store` contract is removed, no deprecation cycle. Publish-side
`outbox.Sender` / `outbox.Store` shape is preserved. chassis-go golden templates migrate
separately, later (§8).

### 7.1 `outbox.Message`

Add ordering fields (filled by the store implementation):

```go
type Message struct {
    ID        string
    Seq       int64          // 0 until sequenced; set on read by the relay store
    Metadata  *event.Metadata
    Data      []byte
    CreateTime time.Time
    // SentTime removed — no completed table
}
```

`tx_start_ts` is a storage concern (set from `@@tidb_current_ts` at INSERT), not surfaced on the
message. There is no `TxOrdinal` — the auto-increment `id` is the within-tx tiebreak (§6).

### 7.2 `relay.Store` (replaces `ListPendingMessages` / `CompletePendingMessages`)

```go
// Store is the sequenced-log store contract.
type Store interface {
    // ListMessages returns sequenced messages with seq > afterSeq, ordered by seq,
    // up to limit.
    ListMessages(ctx context.Context, afterSeq int64, limit int) ([]*outbox.Message, error)

    // Offset returns the last committed seq for the named consumer (0 if none).
    Offset(ctx context.Context, name string) (int64, error)

    // CommitOffset advances the named consumer's watermark. Implementations MUST
    // be monotone (GREATEST semantics) — a lower seq never rewinds the offset.
    CommitOffset(ctx context.Context, name string, seq int64) error
}

// SequencerStore assigns dense seq values to committed-but-unsequenced rows.
// Implementations MUST serialize passes (counter row FOR UPDATE) and order the
// batch by (tx_start_ts, id). Returns the number of rows sequenced.
type SequencerStore interface {
    SequenceMessages(ctx context.Context, limit int) (int, error)
}

// LeaderStore — unchanged.
```

### 7.3 `relay.Relay`

- `NewRelay(store Store, sender eventbus.Sender, opts ...Option)` — `WithName(string)` becomes
  mandatory (consumer group identity; also default leader-lock name).
- `RunOnce`: acquire leadership → sequence pass (if `SequencerStore`) → drain pass → offset
  commit — **all in one tick** (§10).
  Send failure stops the page (stop-the-lane, preserves order) instead of the current
  skip-and-continue; `WithErrorHandler` may override to park-and-continue (DLQ-style) at the
  cost of per-event order.
- Multiple `Relay` instances with different names tail the same log independently. **Every relay
  runs the sequencer pass by default** (counter lock makes extras harmless; keeps each consumer
  group's latency and failure domain self-contained — §10). `WithoutSequencer()` opts a relay out
  for a dedicated-sequencer deployment.
- New `WithRetention(window time.Duration)` enables the sweep (§5.4) on the leader.
- New `WithObserver(Observer)` — dependency-free lag callbacks (§10); mirrors the existing
  `Logger` seam.

### 7.4 Publish side

`Sender.Send` is unchanged. The store implementation fills `tx_start_ts` from `@@tidb_current_ts`
at INSERT and lets TiDB assign `id`; no ordinal bookkeeping and no Sender-lifecycle invariant
(§6). Sends within a tx must reach the store in emit order — guaranteed by the eventbus publisher
calling `Send` synchronously per `Publish`.

## 8. Migration (chassis-go golden templates + services)

Per service cutover, no dual-write:

1. Migration adds the four new tables (`relay_lock` already exists).
2. Deploy version that **publishes to the new `outbox`** and runs **two relays**: legacy relay
   draining `outbox_pending` (old code path) + new sequenced relay. Old table only shrinks.
3. When `outbox_pending` is empty, next release removes the legacy relay and drops
   `outbox_pending` / `outbox_completed`.

Ordering across the cutover boundary is best-effort (two queues drain concurrently for
minutes); services needing a strict boundary can gate step 2 behind a drain-then-switch flag.

markerry-iam `pkg/txoutbox` is the closest ancestor (log + offsets + row-count backStep); it
migrates by adopting the sequencer and dropping `WithBackStep`, and can then delete its local
framework in favor of this package.

## 9. Topics & partitions

Two orthogonal scaling axes, Kafka-mapped:

- **Topics ≈ separate outbox tables** (per domain / event stream). Counter, offsets, lock, and
  sequencer are all per-table already — a store instance just points at a different table name.
  Ordering scoped per table. **Ships fully in v2** (config, not new code): one relay stack per
  table.
- **Partitions ≈ one table + `partition_key`** from a stable hash. **v2 ships the schema, not the
  lanes:** `partition_key` column is present and stamped `hash(subject)` at publish (CloudEvents
  `subject` = aggregate id; overridable via a key-extractor option), but runtime treats the whole
  table as a single partition. Rationale: backfilling a partition column later would force a
  history rewrite or a v3 ordering discontinuity, so the column must exist from day one; the
  parallel-drain machinery is pure runtime and lands later without touching stored data.

Later (partition lanes): one `outbox_sequencer` / `outbox_offsets` row per `(name, partition)`,
one drainer lane per partition, per-key order preserved, cross-partition unordered. One sequencer
pass per table sequences all partitions in a single tx (scan `seq IS NULL` once, bump each
partition's counter) so the §10 latency budget stays one tick regardless of partition count.

## 10. Latency budget & relay runtime

Resolved in the 2026-07-07 grill; this is the contract the runtime is built against.

**SLO:** p99 end-to-end (business COMMIT → consumer handled) **≤ 2s steady-state**, at the default
1s poll. Configurable via `WithPollInterval` — latency-sensitive consumers set 250–500ms and pay
proportionally more idle scans; SpiceDB-style consumers relax to ~5s. Failover is **out of scope
for this p99** (see below) and tracked as a separate availability number.

**Same-tick pipelining (why added latency ≈ 0 vs. today's one-hop model):** within one `RunOnce`
the sequencer tx commits *before* the drain query runs, so a row committed before tick T is
sequenced *and* drained at T. The sequencer does not push events one interval further out.

**Separate batch knobs:** sequencing a row is a cheap `UPDATE seq` (no off-box I/O); draining
sends over the network (10–100× costlier per row). `WithSequenceBatchSize` (default **1000**) is
independent of the drain `WithBatchSize` (default **100**) so a burst sequences far ahead — keeping
the log dense and observable — while draining at a sane send rate.

**Burst / loop-while-full:** both passes loop while their batch comes back full (re-checking
`ctx` between iterations), sleeping the poll interval only on the first short batch. No iteration
cap — each iteration is bounded work, so the 2s p99 holds under any burst the DB can absorb, and
the poll interval degrades to *idle* latency only.

**Every relay sequences (no cross-group coupling):** default-on so a group's lag depends only on
its own interval, and a scaled-to-zero / crashed relay for group A can't stall group B's
sequencing. Cost is one counter-`FOR UPDATE` micro-tx per relay per tick per table — bounded by
groups-per-table, trivial at our scale. `WithoutSequencer()` for a dedicated-sequencer topology.

**Failover:** leadership is a lease. Ungraceful leader loss stalls sequencing + drain for up to
`LeaseTTL` (default lowered to **15s**). Clean shutdown (`SIGTERM`) **releases the lock
explicitly** → planned transitions (deploy/scale-down) are sub-second; only crashes pay the full
TTL. Sub-second failover would need push-based leadership (etcd watch) protoevent-go deliberately
lacks — not pursued. Double-leader windows during lease overlap are safe: counter-`FOR UPDATE`
serializes sequencing, `GREATEST` guards the offset — worst case is duplicate sends, absorbed by
idempotent consumers.

**Observability (`WithObserver`):** dependency-free callback interface (mirrors `Logger`), wired
to Prometheus once in chassis. Two lags: **sequencing lag** (rows with `seq IS NULL`, oldest
`occurred_at`) and **drain lag** per group (`max_assigned_seq − last_seq`, oldest undrained age).
Lag is **derived from data the passes already hold** — the drain page carries `occurred_at`, the
sequencer pass caches `max_assigned_seq` — so idle cost is **2 covered-index scans per tick per
relay, not 3** (no separate `MIN/COUNT`).

**Idle floor:** fixed base interval, **no adaptive backoff** in v2 — it would trade a real
first-event-after-idle latency spike for a saving we don't need at current scale, and complicate
the SLO. Revisit only if a service runs many mostly-idle tables and the empty-scan floor shows up
in DB load.

## 11. Alternatives considered

| alternative | verdict |
|---|---|
| Keep two-table copy-then-delete (status quo) | redundant write/index/sweep; destructive drain blocks multi-consumer & replay |
| In-tx sequencer hot row (`UPDATE seq SET v=v+1` in business tx) | exact, simplest; serializes all publishing txs on one row — wrong default for a shared library, esp. on TiDB pessimistic txs |
| Time-window backStep (cursor + re-scan `create_time > now()-window`) | no extra writes; probabilistic — safe only while tx duration + clock skew < window; re-read volume scales with write rate; acceptable degraded mode, not the default |
| Row-count backStep (markerry txoutbox today) | strictly worse than time-window: invariant depends on throughput (N rows), silently loses events under long tx + burst |
| `TIDB_TRX` safe watermark (advance cursor behind oldest active tx start_ts) | correct-ish; version-coupled system tables, privileges, stalled by any unrelated long tx |
| TiCDC changefeed (commit_ts + resolved-ts, the "real" fix) | requires Kafka/Pulsar/storage sink — no direct app subscription; right answer once a broker exists, rejected for now (no-Kafka constraint) |
| `CREATE SEQUENCE` / `AUTO_ID_CACHE` tuning | does not address the problem: any value allocated pre-commit has the assignment-vs-visibility gap |

## 12. Open questions

- [x] Sequencing latency budget: resolved — see §10. SLO p99 ≤ 2s steady-state at default 1s poll
      (configurable); same-tick sequence→drain pipelining; separate batch knobs (seq 1000 / drain
      100); loop-while-full; every relay sequences by default; `LeaseTTL` 15s + graceful release;
      `WithObserver` lag callbacks; fixed interval, no adaptive backoff.
- [x] `tx_ordinal`: removed. Ordering key is `(tx_start_ts, id)`; the clustered auto-increment
      `id` is the within-tx tiebreak (§6), so there is no ordinal column, no publish-side
      bookkeeping, and no Sender-lifecycle invariant. TiDB-only (no MySQL fallback).
- [x] `UNIQUE(seq)`: dropped. seq uniqueness/density is guaranteed by construction (counter
      `FOR UPDATE`); a unique index would never fire and only adds a second monotonic-write
      hotspot. Guarded by tests + `WithObserver` gap-detection. Index set finalized in §4.
- [x] Versioning: **clean major bump (v2)** of `pkg/transport/outbox`, no deprecation cycle.
      chassis-go template migration deferred — tracked separately, not a blocker for v2.
