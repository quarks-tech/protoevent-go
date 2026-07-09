# Design: Change-Stream Outbox Relay (MongoDB)

Status: IMPLEMENTED (see docs/superpowers/plans/2026-07-07-outbox-v2-mongodb-changestream.md)
Date: 2026-07-07
Owner: @filenko
Companion to: `docs/design/outbox-sequenced-log.md` (TiDB sequenced-log relay)
Supersedes: two-collection (`outbox_pending` / `outbox_completed`) poll+delete relay in the chassis-go MongoDB golden templates

## 1. Problem

The MongoDB outbox in the chassis-go golden templates (`grpc-outbox`, `grpc-derived`,
`crud-lro-publish`) is a poll-based, two-collection, delete-after-send relay driven by the same
`pkg/transport/outbox/relay` engine as TiDB:

- `outbox_pending` — drained FIFO by `_id` (a UUIDv7, time-sortable), then deleted.
- `outbox_completed` — verbatim copy + `sent_time`, TTL-pruned at 7 days.
- `relay_lock` — leader election.

Same defects as the TiDB two-table model: the `outbox_completed` copy is a redundant audit table;
delete-after-send is destructive (one logical consumer, no replay, no second relay); and the
`_id`/UUIDv7 ordering key is assigned at insert, not commit — so any *cursor* over it inherits the
gap where a transaction that minted an earlier UUIDv7 but committed late is skipped. Today the gap
is only masked because delete-after-send re-reads from the start of `outbox_pending`.

**But MongoDB is not SQL.** Unlike TiDB, Mongo offers an application-consumable, commit-ordered,
resumable tail over a collection: **change streams**. They read the oplog, are ordered by commit
`clusterTime`, and are resumable by an opaque token — the Kafka/TiCDC log-tail semantics, with **no
broker** (the driver consumes the cursor directly). This is exactly the property that made us reject
TiCDC for TiDB (it needs Kafka/Pulsar/storage) — and Mongo has it natively.

So on MongoDB we do **not** port the TiDB sequencer. We keep the outbox collection as the durable
event log and replace poll+delete with a change-stream tail. The gap problem structurally cannot
occur, because we consume commit order instead of manufacturing it.

Prerequisites (already true in every Quarks environment — confirmed 2026-07-07): MongoDB runs as a
**replica set** everywhere (dev/CI single-node RS via testcontainers `WithReplicaSet("rs0")`, prod
via the chassis `mongodb` component `replicaSetName`), and the transactional publish already uses
**multi-document transactions** (`session.WithTransaction`). Both change streams and the txn-publish
require a replica set, so this design adds no new infrastructure requirement.

## 2. Goals

- Single insert-only `outbox` collection as the append-only event log; drop `outbox_completed` and
  the `outbox_pending`/delete-after-send split.
- Consume the log via a commit-ordered, resumable **change stream** — no sequencer, no counter, no
  `FOR UPDATE`, no cursor-over-insert-key gap.
- Non-destructive consumption: N independent consumer groups, each with its own resume token; replay
  possible within the oplog + collection-TTL window.
- Ordering equivalent to one Kafka partition: commit-order total order over the collection, causal
  order preserved, at-least-once delivery with idempotent consumers.
- Reuse the cross-cutting relay primitives shared with the TiDB `sequence` runtime (leader election,
  observability, error policy, `Sender`, `Message`) rather than duplicating them.

Non-goals:

- Exactly-once delivery (consumers dedup on `event_id`, as today).
- Ordering between genuinely concurrent transactions (Kafka parity).
- Exact-order recovery after a resume token falls off the oplog (§7 — best-effort, break-glass).
- A generalized single `Store` interface across TiDB and Mongo (§3 — deliberately two runtimes).

## 3. Package structure (shared with the TiDB design)

The two backends have genuinely different mechanics — TiDB *manufactures* order (poll a page + assign
a sequence), Mongo *consumes* native commit order (push via a blocking cursor). Forcing one `Store`
interface over both would mean a union offset type (`int64 seq` vs opaque resume token) and dead
methods (`SequenceMessages` is meaningless on Mongo). So we keep **two runtimes** and share only the
cross-cutting concerns:

```
pkg/transport/outbox/relay/          # shared primitives — no Relay type
    Observer, Logger, LeaderStore, LeaderElector helper, error policy (stop-the-lane / park)
pkg/transport/outbox/relay/sequence/ # sequence.Relay, Store, SequencerStore, RetentionStore   (TiDB)
pkg/transport/outbox/relay/stream/   # stream.Relay, StreamStore                                (Mongo)
```

- **`relay` (shared):** `Observer`, `Logger`, `LeaderStore` (both backends use a `relay_lock`-style
  lease), a `LeaderElector` helper (acquire / renew / graceful release), and the error policy
  (stop-the-lane default vs park-and-continue via an error handler). These are identical concerns.
- **`relay/sequence`:** the TiDB runtime and its `Store` / `SequencerStore` / `RetentionStore`
  (int64 `seq`). See the companion doc.
- **`relay/stream`:** the Mongo runtime and its `StreamStore` (opaque resume token, `Watch`). This
  doc.

> This restructure renames the TiDB design's `relay.Relay` → `sequence.Relay`, `relay.Store` →
> `sequence.Store`, etc., moving `Observer` / `Logger` / `LeaderStore` / error policy up into `relay`.
> The TiDB implementation plan's package paths must be patched accordingly (it is not yet executed).

## 4. Collection schema

The `outbox` collection keeps the current envelope shape (metadata-as-JSON-bytes + data), and becomes
**insert-only**. `outbox_completed` is removed.

```go
// outbox — insert-only append-only event log, tailed by change stream.
type outboxDoc struct {
    ID         string    `bson:"_id"`          // message UUID (natural insert dedup)
    Metadata   []byte    `bson:"metadata"`     // CloudEvents metadata as JSON bytes
    Data       []byte    `bson:"data"`
    CreateTime time.Time `bson:"create_time"`  // insert time; TTL + best-effort DR re-read key
}

// outbox_offsets — one doc per consumer group (the resume-token store).
type offsetDoc struct {
    Name        string    `bson:"_id"`           // consumer group name
    ResumeToken []byte    `bson:"resume_token"`  // opaque BSON binary; stored as []byte (NOT bson.Raw:
                                                 // bson.Raw requires valid BSON-document bytes and fails to
                                                 // marshal an arbitrary/opaque token — []byte binary round-trips
                                                 // any bytes, and a real resume token still reconstructs via
                                                 // bson.Raw(string(stored)) for SetResumeAfter)
    ClusterTime time.Time `bson:"cluster_time"`  // commit clusterTime of last processed event (DR anchor)
    UpdateTime  time.Time `bson:"update_time"`
}

// relay_lock — unchanged (one active consumer per group).
type lockDoc struct {
    Name       string    `bson:"_id"`
    HolderID   string    `bson:"holder_id"`
    ExpireTime time.Time `bson:"expire_time"`
}
```

**Indexes:**
- `outbox`: implicit unique `_id` (message UUID → natural insert dedup); **TTL index on
  `create_time`** with `expireAfterSeconds` ≥ the oplog window (§7). No `outbox_completed`, no
  delete-after-send.
- `outbox_offsets`: implicit `_id` (consumer name).
- `relay_lock`: implicit `_id` (lock name).

## 5. Lifecycle

### 5.1 Publish — inside the business transaction (unchanged)

The publish path is identical to today: an `InsertOne` into `outbox` on the session-bound context, so
the event commits atomically with the business write. No `seq`, no ordering key to compute — commit
order is assigned by the oplog at commit.

```go
doc := outboxDoc{ID: msg.ID, Metadata: meta, Data: msg.Data, CreateTime: msg.CreateTime}
_, err := coll.InsertOne(sessCtx, doc) // sessCtx is the business txn's session context
```

### 5.2 Consume — `stream.Relay` runtime

One leader per consumer group (else duplicate sends). The leader opens a change stream resumed from
the group's stored token and drains it. The loop uses `maxAwaitTimeMS` so `TryNext` returns
periodically even with no events — that tick is what renews the leader lease, batches the token
persist, and checks `ctx`.

```
loop:
  renew leader lease (LeaderElector); if lost → stop draining, idle
  open/continue change stream:
      pipeline = [{$match: {operationType: "insert"}}]     # ignore TTL deletes (§6a)
      resumeAfter = stored token        (or startAtOperationTime="now" for a new group — §6b)
      maxAwaitTimeMS = drain window
  for each event in this window:
      msg = decode(event.fullDocument)
      Sender.Send(msg)                  # per-event
      on send failure, NO error handler → STOP-THE-LANE (see below)
      on send failure, error handler set → park-and-continue (advance past)
      advance in-memory token = event._id (resume token), clusterTime = event.clusterTime
  persist (token, clusterTime) ONCE for the window   # batch cadence (§6c)
  on invalidate event → fatal: log + ObserveError + stop (§6d)
  on ChangeStreamHistoryLost → break-glass DR (§7)
```

**Stop-the-lane must close and reopen the cursor (a change-stream cursor cannot rewind).** Unlike
the TiDB drain — which re-queries `ListMessages` from the uncommitted offset every tick, so a
non-advanced offset naturally redelivers the failed row — a change stream holds a *live cursor* that
has already discarded the failed event. Simply breaking the drain loop and looping on the same
cursor would fast-forward past the failed event and advance the token, silently dropping it. So on
stop-the-lane the relay **closes the stream and backs off**; the next window reopens via
`LoadToken` + `Watch(resumeAfter = last-persisted token)`, which resumes *just after* the last
successful send — i.e. at the failed event — and redelivers it. Persisted state makes this exact:
a mid-window stop already saved the last-success token (§6c), and a first-event stop saved nothing
(reopen resumes from the prior token). Park-and-continue, by contrast, keeps the same cursor open
and advances past the parked event (order-relaxing by design). The persistence rule alone (§6c) only
covered crash+restart; this close-on-stop rule is what makes stop-the-lane correct while the process
stays up.

### 5.3 Retention

The `outbox` TTL index prunes by `create_time`. No relay-side deletion. TTL window must exceed the
oplog window so the break-glass DR re-read (§7) still finds events after a token loss.

## 6. Stream configuration (decisions)

- **(a) What to watch:** pipeline `[{$match: {operationType: "insert"}}]`. The collection is
  insert-only in normal operation, but TTL pruning emits `delete` change events — the match filter
  excludes them so the relay never tries to "deliver" a deletion. Insert change events already carry
  the full document, so no `fullDocument` lookup is configured.
- **(b) New-consumer start position (v1: "now" only):** a new consumer group (no stored token)
  starts at **"now"** (`startAtOperationTime` = current cluster time) — it subscribes to future
  events, like Kafka `auto.offset.reset=latest`, and does not re-emit the existing outbox. v1 has
  **no** replay-from-beginning option: replay-from-beginning and the break-glass DR re-read (§7) are
  the same gapless-scan-then-stream routine and are **deferred together** as a future "backfill"
  feature (§7). Building only one of them would build ~90% of the other, so v1 ships neither.
- **(c) Token persistence cadence:** batch — persist once per drain window (`WithDrainWindow`,
  default 1s, see §8.2) or every N events, whichever comes first. **Which token depends on whether
  the window had matching events:**
  - **Non-empty window:** persist the **last successfully-sent** event's resume token + its
    `clusterTime`. On a stop-the-lane failure, persist up to the last success.
  - **Empty window (caught up):** persist the **postBatchResumeToken (PBRT)** + its `clusterTime`.
    MongoDB advances the PBRT even when no events match the pipeline, so persisting it keeps a
    caught-up-and-*connected* consumer resumable indefinitely (its stored position tracks the oplog
    head and never falls off) and keeps the DR/lag anchor fresh.

  Redelivery on crash = one window, absorbed by `event_id` dedup. Not per-event (avoids a token
  round-trip per event). This split is also what makes committed-token age an honest lag signal
  (§7): it stays ≈0 when healthy, and grows only when the consumer genuinely lags (a slow consumer
  persists the lagging last-processed token, never the fresh PBRT).
- **(d) Invalidate events:** if `outbox` is dropped/renamed the stream emits `invalidate` and closes.
  Treated as a **fatal** relay error — log via `Logger`, report via `Observer.ObserveError`, stop the
  relay. Do not silently reopen from "now" (that would skip history). Surfaces a misconfiguration.

## 7. Ordering, dedup, and the resume-token cliff

**Steady state.** Change streams deliver in commit `clusterTime` order, gap-free, resumable by token.
The insert-time-vs-commit-time gap that the TiDB sequencer exists to close simply does not arise —
we consume commit order rather than manufacture it. Causal order is preserved (a later-committing,
causally-dependent event has a later `clusterTime`); concurrent-transaction order is unspecified
(Kafka parity). Delivery is at-least-once; consumers dedup on the CloudEvents `event_id` (and the
collection's unique `_id` gives natural insert-side dedup).

**The cliff (the change-stream analog of "the gap").** A resume token — and `startAtOperationTime` —
is only usable while its position is still in the oplog. If a consumer group is down or lagging
**longer than the oplog window**, the stream reopen fails with `ChangeStreamHistoryLost`; token *and*
operation-time resume are both dead. Recovery must then come from the durable `outbox` collection —
and re-reading it gap-free would require a commit-ordered key stamped in each doc, which Mongo
**cannot** provide at insert time (commit `clusterTime` is unknowable inside the txn, exactly as
`tx_start_ts` could not be commit order on TiDB). So a DR re-read is inherently best-effort-ordered.

**Decision (2026-07-07): best-effort DR, shipped as a documented break-glass procedure + lag
alerting — not built into `stream.Relay` v1.**

- v1 `stream.Relay` reports **committed-token age** (`now − clusterTime(committed token)`) via
  `Observer.ObserveDrained`'s `oldestAge` — computed with no extra query (we already hold the
  token's `clusterTime`) and **no `local.oplog.rs` read permission** (which the app's Mongo user
  likely lacks). Thanks to the PBRT persistence rule (§6c) this stays ≈0 while healthy and grows
  only when the consumer genuinely lags. Operators alert on it approaching the **statically
  configured oplog window** (a known deployment constant) — e.g. page at ~50%. True oplog headroom
  (querying `local.oplog.rs` for the oldest entry) was rejected as the metric: extra query + a
  permission we don't want to require, when this cheap proxy suffices.
- On `ChangeStreamHistoryLost`, v1 stops with a fatal error (like invalidate) and the runbook is
  invoked. It does **not** auto-recover.
- **Break-glass runbook:** re-read `outbox` where `create_time >= lastClusterTime - overlap`
  (from the persisted `cluster_time` anchor), re-send all (consumers dedup on `event_id`), then open
  a fresh stream at "now." Order during this catch-up is best-effort (`create_time`); steady-state
  order stays exact. This is the TiDB backstep tradeoff, quarantined to a disaster path that should
  never fire if the windows below are sized correctly.
- **Operationally make it "never" fire:** size **`outbox` TTL (7d) > oplog window >
  consumer-downtime SLO** (matching §4's requirement that the TTL outlive the oplog window, so the
  break-glass re-read still finds events after a token loss), and alert before lag reaches the oplog
  window.

**Future "backfill" feature (deferred — unifies DR + replay).** Both the break-glass DR re-read
above and a replay-from-beginning start position (§6b) are the *same* routine: a gapless
scan-then-stream — open the change stream first to capture a resume point at "now", scan the
`outbox` collection (best-effort `create_time` order), forward with `event_id` dedup, then consume
the stream from the captured point so events inserted during the scan are not missed. When built, it
is **one** routine parameterized by an optional lower-bound `clusterTime`: no bound = replay from
beginning; `anchor − overlap` = DR recovery. v1 builds neither; break-glass stays the manual runbook
and there is no replay start position.

## 8. protoevent-go changes

New module: `pkg/transport/outbox/mongodb` (own go.mod — heavy driver/test deps stay out of the
engine, mirroring `pkg/transport/outbox/tidb`). Depends on `go.mongodb.org/mongo-driver/v2` (already
the org standard, v2.5–2.7).

### 8.1 Shared `relay` package (extracted; see §3)

```go
package relay

type Observer interface {
    ObserveDrained(name string, count int, oldestAge time.Duration, more bool)
    ObserveError(name string, err error)
    // ObserveSequenced stays in relay/sequence — it is sequence-specific.
}

type Logger interface{ Errorf(format string, args ...any) }

type LeaderStore interface {
    TryAcquireLeaderLock(ctx context.Context, name, holderID string, ttl time.Duration) (bool, error)
    ReleaseLeaderLock(ctx context.Context, name, holderID string) error
}

// LeaderElector wraps a LeaderStore with acquire/renew/graceful-release; used by both runtimes.
```

### 8.2 `relay/stream` — runtime contract

```go
package stream

// StreamStore is the change-stream read/offset contract, implemented over MongoDB.
//
// The resume token crosses this boundary as an opaque string, NOT bson.Raw —
// Go strings hold arbitrary bytes, so a BSON token fits, and string gives
// immutability (callers can't mutate a held token), comparability (==, map keys),
// and a clean API value. The mongo store *persists* the token as []byte binary
// (offsetDoc.ResumeToken, §4) — never as bson.Raw, which requires valid
// BSON-document bytes and would reject an opaque token. bson.Raw appears only
// transiently at the driver API surface — SetResumeAfter(bson.Raw(token)) when
// opening Watch, and decoding a change event's _id — cast to/from string right
// at that edge; it is never the stored or StreamStore-crossing representation.
// This keeps the engine `relay/stream` package free of the mongo driver
// (dependency-free core) and the vocabulary backend-neutral (§10 Kafka note).
// "No stored token → start now" is token == "".
type StreamStore interface {
    // LoadToken returns the consumer group's stored resume token ("" if none —
    // a new group then starts at "now") and the anchor clusterTime.
    LoadToken(ctx context.Context, name string) (token string, clusterTime time.Time, err error)

    // SaveToken persists the latest successfully-processed resume token + clusterTime
    // for the consumer group. Called once per drain window (§6c).
    SaveToken(ctx context.Context, name string, token string, clusterTime time.Time) error

    // Watch opens a change stream on the outbox collection, filtered to inserts,
    // resumed from token (or from "now" when token is "" — v1 has no
    // replay-from-beginning). The returned Stream yields decoded messages plus
    // their resume token + clusterTime, and exposes the PBRT for empty windows.
    Watch(ctx context.Context, token string) (Stream, error)
}

// Stream is a live change-stream cursor. Next blocks up to the drain window
// (maxAwaitTimeMS) and returns (nil, ok=false) on window timeout with no event;
// on such an empty window PBRT() returns the advanced postBatchResumeToken so a
// caught-up consumer's persisted position keeps tracking the oplog head (§6c).
type Stream interface {
    Next(ctx context.Context) (*Event, bool, error)
    PBRT() (token string, clusterTime time.Time) // postBatchResumeToken after an empty window
    Close(ctx context.Context) error
}

type Event struct {
    Message     *outbox.Message
    ResumeToken string
    ClusterTime time.Time
    Invalidate  bool // true on an invalidate change event → fatal (§6d)
}

// Relay tails the outbox change stream for one consumer group and forwards to a Sender.
type Relay struct { /* name, store StreamStore, sender eventbus.Sender, shared relay opts */ }

func NewRelay(name string, store StreamStore, sender eventbus.Sender, opts ...Option) *Relay
func (r *Relay) Run(ctx context.Context) error // leader-gated stream loop with graceful release
```

Options mirror the shared set where they apply: `WithLeaseTTL` (default 15s), `WithLeaderLockName`,
`WithObserver`, `WithLogger`, `WithErrorHandler` (park-and-continue; default stop-the-lane),
`WithDrainWindow(d)`, `WithTokenBatchSize(n)`. No `SequenceBatchSize`, no `RetentionStore` (TTL
index handles pruning), and **no start-position option in v1** (always "now"; replay is the deferred
backfill feature, §7).

`WithDrainWindow(d)` sets the change stream's `maxAwaitTimeMS` (default **1s**). It is a **separate
knob from the sequence relay's `PollInterval`** — they are not the same concept: `PollInterval` is
an idle-poll interval, `WithDrainWindow` is the cursor's max blocking wait (an available event is
delivered immediately; the window only bounds the idle wait, and is when the loop wakes to renew
the lease, batch-persist the token, and re-check `ctx`). `NewRelay` **validates
`DrainWindow < LeaseTTL/2`** (so the lease can always be renewed within a window) and rejects a
misconfiguration.

**Hardening note — lease renewal is between `RunOnce` calls, not within a drain window.** The lease
is renewed once per `RunOnce` call (§5.2's "renew leader lease" step), but a single `drainWindow` can
process up to `TokenBatchSize` events, each sent **synchronously**. If `Sender.Send` is slow, one
`drainWindow` call can take longer than `LeaseTTL` even though `DrainWindow < LeaseTTL/2` bounds only
the *idle* wait, not the sum of `TokenBatchSize` synchronous sends. A transient second leader can then
acquire the lease and drain an overlapping range while the first is still mid-window. This does **not**
violate at-least-once (the consumer's `event_id` dedup absorbs the overlap), but it weakens the
single-active-consumer property. Operators should size `TokenBatchSize × worst-case Sender.Send
latency < LeaseTTL` to keep a single window inside one lease term.

### 8.3 `pkg/transport/outbox/mongodb` — MongoDB `StreamStore` + publish

- `Store` implementing `outbox.Store` (publish `InsertOne`), `stream.StreamStore` (`LoadToken` /
  `SaveToken` on `outbox_offsets`, `Watch` opening the change stream with the insert-match pipeline
  and `SetMaxAwaitTime` / `SetResumeAfter` / `SetStartAtOperationTime`), and `relay.LeaderStore`
  (the existing conditional-upsert `relay_lock` logic).
- `Watch` decodes each change event's `fullDocument` into `outbox.Message` (JSON-unmarshal the
  `metadata` bytes back into `event.Metadata`, as today), and surfaces the change event `_id`
  (resume token) + `clusterTime`.
- Testcontainers single-node replica set (`mongodb.WithReplicaSet("rs0")` + `directConnection=true`)
  for integration tests.

### 8.4 Publish side (unchanged)

`outbox.Sender` / `NewPublisherFactory` are unchanged. The MongoDB `Store.CreateOutboxMessage` is
called on the session-bound context inside `WithTransaction`, exactly as today.

## 9. Migration (chassis-go golden templates + services)

Per service cutover, no dual-write, mirroring the TiDB plan:

1. Migration: add the `outbox_offsets` collection and a TTL index on `outbox.create_time`; keep the
   existing `outbox_pending` draining under the legacy poll relay for now.
2. Deploy a version that **publishes to the insert-only `outbox`** (drop the `sent_time` write) and
   runs the new `stream.Relay` alongside the legacy poll relay draining any residual
   `outbox_pending`. New events flow through the change stream; the old collection only shrinks.
3. When `outbox_pending` is empty, next release removes the legacy poll relay and drops
   `outbox_pending` / `outbox_completed`.

Because the new relay starts at "now", the cutover must **open the `stream.Relay` before flipping
publish** to the new `outbox`: once the stream is watching, no events are published to the new
collection until the publish flip, so nothing is missed. (v1 has no replay-from-beginning fallback —
that is the deferred backfill feature, §7 — so this ordering is required, not optional.)

chassis-go template changes are a separate, later effort — not in scope here.

## 10. Alternatives considered

| alternative | verdict |
|---|---|
| Port the TiDB sequencer to Mongo (counter doc + `findAndModify`, poll) | works but pointless — reintroduces a hot counter doc and a poll loop to manufacture an order Mongo already provides commit-ordered for free via change streams |
| Keep poll + delete-after-send (status quo) | redundant `outbox_completed`; destructive drain blocks multi-consumer & replay; `_id`/UUIDv7 cursor would inherit the insert-vs-commit gap |
| Watch domain collections directly (Debezium-on-Mongo) | drops the collection but couples event schema to document schema, loses the typed CloudEvents envelope, can't represent events that aren't 1:1 document mutations |
| Generalize one `Store` interface across TiDB + Mongo (opaque `[]byte` offset, one runtime) | union offset type + dead methods (`SequenceMessages` meaningless on Mongo); the two mechanics (poll+sequence vs push+resume-token) are different enough that a unified interface adds indirection, not clarity |
| TiCDC-style external CDC → Kafka | needs a broker; the whole point of change streams is the app consumes commit-ordered CDC directly, no broker |
| Auto-recover exact order after oplog loss | impossible without a commit-ordered key in the doc, which Mongo can't stamp at insert; best-effort break-glass is the honest ceiling |
| Genericize `StreamStore` so Kafka can be the relay *source* | rejected — the shape rhymes (resumable cursor + poll + commit ≈ Kafka consumer), but Kafka's own consumer-group protocol already provides one-consumer-per-partition (our leader election goes unused), the broker owns the committed offset (`LoadToken`/`SaveToken` become thin delegates), and with Kafka as source there is no transactional DB / `outbox` collection / atomic-with-business-write — it is a Kafka→Sender bridge, a different product. Kafka *as the relay SINK* is already supported with zero changes: the relay forwards to any `eventbus.Sender`, so a Kafka-backed Sender needs no runtime change. Design hygiene: name `stream` package concepts around "resumable stream position," not "MongoDB resume token," so the vocabulary generalizes even though the implementation stays Mongo-shaped in v1. |

## 11. Open questions

- [x] Drain window default: resolved — separate `WithDrainWindow(d)` knob (not tied to
      `PollInterval`), default **1s**, with a `NewRelay` guard `DrainWindow < LeaseTTL/2`. See §6a/§8.2.
- [x] `metadata` encoding: resolved — **keep JSON bytes** (clean CloudEvents round-trip for
      `Extensions`/`url.URL`; the relay never queries metadata, so native BSON buys unused
      queryability + a round-trip hazard). Future escape hatch if server-side change-stream pipeline
      filtering (topic-like per-`type` streams) is ever wanted: **hybrid** — promote `type` (± `subject`)
      to denormalized native top-level BSON fields for `$match`, keep full metadata as JSON bytes.
      Not built in v1 (no consumer needs per-type stream filtering).
- [x] `StartFromBeginning`: resolved — deferred out of v1 (StartNow-only). Replay-from-beginning and
      break-glass DR are the same gapless scan-then-stream routine and are unified into one deferred
      "backfill" feature (§7). Building one would build ~90% of the other, so v1 ships neither.
- [x] Lag metric: resolved — **committed-token age** (`now − clusterTime(committed token)`),
      exposed via `Observer.ObserveDrained`'s `oldestAge`, no extra query, no `local` permission;
      alert against the statically configured oplog window. Made honest by the PBRT persistence
      rule (§6c: PBRT on empty windows, last-processed token otherwise). See §7.
