package tidb_test

import (
	"context"
	"database/sql"
	"reflect"
	"sync"
	"testing"

	"github.com/quarks-tech/protoevent-go/pkg/event"
	"github.com/quarks-tech/protoevent-go/pkg/transport/outbox"
	"github.com/quarks-tech/protoevent-go/pkg/transport/outbox/relay/sequence"
	tidb "github.com/quarks-tech/protoevent-go/pkg/transport/outbox/tidb"
)

// recordingSender records the CloudEvents id of every delivered event, in
// delivery order. Because the relay is driven by a sequenced log drained in
// Seq order, delivery order is the proof of ordering; because the default
// outbox.RowIDGenerator is ReuseMetadataID (the outbox row's event_id equals the
// publisher's Metadata.ID, preserved end to end), comparing the recorded IDs
// against the published IDs also proves CloudEvents-id fidelity, not just
// position/count.
type recordingSender struct {
	mu  sync.Mutex
	ids []string // delivered event IDs, in delivery (i.e. Seq) order
}

func (r *recordingSender) Send(_ context.Context, md *event.Metadata, _ []byte) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ids = append(r.ids, md.ID)
	return nil
}

// TestRelayEndToEndOrderAndDelivery proves the latest-default end to end: the
// consumer group is primed (one RunOnce on the empty log, which initializes
// its offset at "latest") BEFORE anything is published, so the subsequent 50
// publishes are all newer than the group's start and must all be delivered,
// in order.
func TestRelayEndToEndOrderAndDelivery(t *testing.T) {
	if testDB == nil {
		t.Skip("no TiDB")
	}
	truncate(t)

	sender := &recordingSender{}
	// Single relay instance: it both sequences and drains.
	r, err := sequence.NewRelay("e2e", tidb.NewRelayStore(testDB), sender,
		sequence.WithBatchSize(10), sequence.WithSequenceBatchSize(1000))
	if err != nil {
		t.Fatalf("NewRelay: %v", err)
	}

	// Prime the group on the empty log: this is where latest-default
	// initializes the offset (to 0, since nothing is sequenced yet).
	if err := r.RunOnce(context.Background()); err != nil {
		t.Fatalf("priming RunOnce: %v", err)
	}
	if len(sender.ids) != 0 {
		t.Fatalf("priming delivered %d, want 0", len(sender.ids))
	}

	wantIDs := make([]string, 0, 50)
	for range 50 {
		wantIDs = append(wantIDs, publish(t, "s"))
	}

	if err := r.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	if len(sender.ids) != 50 {
		t.Fatalf("delivered %d, want 50", len(sender.ids))
	}
	// The delivered IDs, in order, must exactly match the published IDs, in
	// publish order: this is the ordering + at-least-once + id-fidelity proof.
	if !reflect.DeepEqual(sender.ids, wantIDs) {
		t.Fatalf("delivered IDs =\n%v\nwant (published order)\n%v", sender.ids, wantIDs)
	}
	// Offset advanced to 50.
	off, _, err := tidb.NewRelayStore(testDB).Offset(context.Background(), "e2e")
	if err != nil {
		t.Fatal(err)
	}
	if off != 50 {
		t.Fatalf("offset = %d, want 50", off)
	}
}

// TestRelayStartFromBeginningReplays proves the explicit replay opt-in: a
// fresh group with WithStartFromBeginning delivers events published BEFORE it
// ever ran, unlike the latest-default proven above.
func TestRelayStartFromBeginningReplays(t *testing.T) {
	if testDB == nil {
		t.Skip("no TiDB")
	}
	truncate(t)

	wantIDs := make([]string, 0, 5)
	for range 5 {
		wantIDs = append(wantIDs, publish(t, "s"))
	}

	sender := &recordingSender{}
	r, err := sequence.NewRelay("replay", tidb.NewRelayStore(testDB), sender,
		sequence.WithStartFromBeginning())
	if err != nil {
		t.Fatalf("NewRelay: %v", err)
	}

	if err := r.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	if len(sender.ids) != 5 {
		t.Fatalf("delivered %d, want 5 (WithStartFromBeginning must replay the retained log)", len(sender.ids))
	}
	if !reflect.DeepEqual(sender.ids, wantIDs) {
		t.Fatalf("delivered IDs =\n%v\nwant (published order)\n%v", sender.ids, wantIDs)
	}
}

// TestRelayRestartAfterPrimingDeliversPending is the regression test for a
// live-reproduced silent event-loss bug: a new group primed on an EMPTY log
// commits an offset row at 0; the old GREATEST-upsert InitOffsetLatest, re-run
// after a relay restart (fresh in-memory latch), forward-jumped that committed
// row 0 → MAX(seq) and silently skipped everything published while the relay
// was down. Insert-if-absent must leave the committed row untouched, so the
// restarted relay delivers all pending events.
func TestRelayRestartAfterPrimingDeliversPending(t *testing.T) {
	if testDB == nil {
		t.Skip("no TiDB")
	}
	truncate(t)
	ctx := context.Background()

	// First relay instance: prime the new group on the empty log — this
	// commits its offset row at 0.
	primer := &recordingSender{}
	r1, err := sequence.NewRelay("restart", tidb.NewRelayStore(testDB), primer)
	if err != nil {
		t.Fatalf("NewRelay (first instance): %v", err)
	}
	if err := r1.RunOnce(ctx); err != nil {
		t.Fatalf("priming RunOnce: %v", err)
	}
	if len(primer.ids) != 0 {
		t.Fatalf("priming delivered %d, want 0", len(primer.ids))
	}

	// Simulate the process dying: RunOnce holds the leader lease (only Run
	// releases it on shutdown), so drop the lock the way an expired lease
	// would, letting the second instance take leadership immediately.
	if _, err := testDB.ExecContext(context.Background(), "DELETE FROM relay_locks"); err != nil {
		t.Fatalf("expire leader lock: %v", err)
	}

	// Publish while the relay is "down".
	wantIDs := make([]string, 0, 20)
	for range 20 {
		wantIDs = append(wantIDs, publish(t, "pending"))
	}

	// Second relay instance for the SAME group: its fresh in-memory latch
	// re-runs InitOffsetLatest against the existing committed row at 0.
	sender := &recordingSender{}
	r2, err := sequence.NewRelay("restart", tidb.NewRelayStore(testDB), sender)
	if err != nil {
		t.Fatalf("NewRelay (second instance): %v", err)
	}
	if err := r2.RunOnce(ctx); err != nil {
		t.Fatalf("post-restart RunOnce: %v", err)
	}

	if len(sender.ids) != 20 {
		t.Fatalf("delivered %d, want 20 (InitOffsetLatest must not forward-jump a committed offset row)", len(sender.ids))
	}
	if !reflect.DeepEqual(sender.ids, wantIDs) {
		t.Fatalf("delivered IDs =\n%v\nwant (published order)\n%v", sender.ids, wantIDs)
	}
	off, _, err := tidb.NewRelayStore(testDB).Offset(ctx, "restart")
	if err != nil {
		t.Fatal(err)
	}
	if off != 20 {
		t.Fatalf("offset = %d, want 20", off)
	}
}

func TestLatePublishGetsHigherSeq(t *testing.T) {
	if testDB == nil {
		t.Skip("no TiDB")
	}
	truncate(t)
	publish(t, "early")

	st := tidb.NewRelayStore(testDB)
	ctx := context.Background()
	if _, err := st.SequenceMessages(ctx, 100); err != nil {
		t.Fatal(err)
	}

	// A row published AFTER the first sequence pass must get a strictly higher
	// seq — never hide below the watermark. This is the gap the design closes.
	publish(t, "late")
	if _, err := st.SequenceMessages(ctx, 100); err != nil {
		t.Fatal(err)
	}

	// Go through the public API (ListMessages) rather than a raw JSON_EXTRACT
	// query, so this test doesn't couple to the metadata storage encoding.
	msgs, err := st.ListMessages(ctx, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 2 {
		t.Fatalf("got %d messages, want 2", len(msgs))
	}
	var earlySeq, lateSeq int64
	for _, m := range msgs {
		switch m.Metadata.Subject {
		case "early":
			earlySeq = m.Seq
		case "late":
			lateSeq = m.Seq
		default:
			t.Fatalf("unexpected subject %q", m.Metadata.Subject)
		}
	}
	if earlySeq == 0 || lateSeq == 0 {
		t.Fatalf("did not find both subjects: early seq=%d late seq=%d", earlySeq, lateSeq)
	}
	if lateSeq <= earlySeq {
		t.Fatalf("late seq %d not > early seq %d", lateSeq, earlySeq)
	}
}

// TestSameAggregateSeqFollowsBeginOrderNotCommitOrder pins what the sequenced log
// actually orders by, using the case the design doc used to excuse.
//
// docs/design/outbox-sequenced-log.md disclaimed ordering only for "genuinely
// concurrent transactions", and excused the dangerous case with: transactions that
// matter to each other (same aggregate) "conflict on row locks, serialize, and fall
// under the theorem" — where the theorem requires B to have STARTED after A
// COMMITTED.
//
// That escape clause does not hold, and this test is the counterexample. Two
// transactions here conflict on one row and do serialize on the lock, but their
// lifetimes OVERLAP: A takes its start TSO first, B then takes the row lock and
// commits, and only then does A write the same row and commit. seq follows
// tx_start_ts (@@tidb_current_ts, allocated at BEGIN — see CreateOutboxMessage), so
// A is delivered first while A is the last writer.
//
// Verified on TiDB v7.5.1: delivery order A,B with the database holding A, so a
// last-write-wins replay reaches B and diverges from the source of truth
// permanently. Nothing is duplicated, so event_id dedup cannot repair it.
//
// The guarantee this asserts is therefore the honest one — seq is ascending in
// transaction-BEGIN order — and NOT "commit order". A consumer that needs
// last-write-wins semantics per aggregate must carry its own version/revision in
// the payload and reject stale writes; the log's order alone is not enough. Both
// docs were corrected to say so.
func TestSameAggregateSeqFollowsBeginOrderNotCommitOrder(t *testing.T) {
	if testDB == nil {
		t.Skip("no TiDB")
	}
	truncate(t)
	ctx := context.Background()

	// A one-row business aggregate, in the same database as the outbox so both
	// writes are in one transaction — the whole point of an outbox.
	if _, err := testDB.ExecContext(ctx,
		"CREATE TABLE IF NOT EXISTS causal_aggregate (k VARCHAR(16) PRIMARY KEY, v VARCHAR(16) NOT NULL)"); err != nil {
		t.Fatalf("create aggregate table: %v", err)
	}
	t.Cleanup(func() { _, _ = testDB.ExecContext(ctx, "DROP TABLE IF EXISTS causal_aggregate") })
	if _, err := testDB.ExecContext(ctx,
		"INSERT INTO causal_aggregate (k, v) VALUES ('k', 'init') ON DUPLICATE KEY UPDATE v = 'init'"); err != nil {
		t.Fatalf("seed aggregate: %v", err)
	}

	// publishIn writes one event through the production Sender inside tx.
	publishIn := func(tx *sql.Tx, value string) string {
		md := newTestMetadata("causal-" + value)
		if err := outbox.NewSender(tidb.NewStore(tx)).Send(ctx, md, []byte(value)); err != nil {
			t.Fatalf("publish %s: %v", value, err)
		}

		return md.ID
	}

	txA, err := testDB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin A: %v", err)
	}
	defer func() { _ = txA.Rollback() }()

	// Materialize A's start TSO BEFORE B exists. This read takes no lock, so it
	// makes A "older" without serializing anything.
	var startA uint64
	if err := txA.QueryRowContext(ctx, "SELECT @@tidb_current_ts").Scan(&startA); err != nil {
		t.Fatalf("read A start ts: %v", err)
	}

	// B begins later, takes the row lock, writes 'B', publishes, and COMMITS FIRST.
	txB, err := testDB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin B: %v", err)
	}
	var startB uint64
	if err := txB.QueryRowContext(ctx, "SELECT @@tidb_current_ts").Scan(&startB); err != nil {
		t.Fatalf("read B start ts: %v", err)
	}
	if _, err := txB.ExecContext(ctx, "UPDATE causal_aggregate SET v = 'B' WHERE k = 'k'"); err != nil {
		t.Fatalf("B update: %v", err)
	}
	idB := publishIn(txB, "B")
	if err := txB.Commit(); err != nil {
		t.Fatalf("commit B: %v", err)
	}

	// Now A writes the SAME row. It conflicts with B — the serialization the design
	// doc relied on — and commits LAST, so the database's final value is A's.
	if _, err := txA.ExecContext(ctx, "UPDATE causal_aggregate SET v = 'A' WHERE k = 'k'"); err != nil {
		t.Fatalf("A update: %v", err)
	}
	idA := publishIn(txA, "A")
	if err := txA.Commit(); err != nil {
		t.Fatalf("commit A: %v", err)
	}

	if startA >= startB {
		t.Fatalf("start TSOs did not order A before B (A=%d B=%d); this test is not exercising "+
			"the overlap it claims", startA, startB)
	}
	var dbValue string
	if err := testDB.QueryRowContext(ctx, "SELECT v FROM causal_aggregate WHERE k = 'k'").Scan(&dbValue); err != nil {
		t.Fatalf("read aggregate: %v", err)
	}
	if dbValue != "A" {
		t.Fatalf("database value = %q, want \"A\" (A must be the last writer); the intended "+
			"interleaving did not happen", dbValue)
	}

	sender := &recordingSender{}
	r, err := sequence.NewRelay("causal", tidb.NewRelayStore(testDB), sender,
		sequence.WithStartFromBeginning())
	if err != nil {
		t.Fatalf("NewRelay: %v", err)
	}
	if err := r.RunOnce(ctx); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	// THE guarantee: seq order is transaction-BEGIN order. A began first, so A is
	// delivered first — even though A committed last.
	if !reflect.DeepEqual(sender.ids, []string{idA, idB}) {
		t.Fatalf("delivered %v, want [%s %s] — seq must be ascending in tx_start_ts "+
			"(transaction-begin order)", sender.ids, idA, idB)
	}

	// And the consequence, asserted so it cannot quietly stop being true: begin order
	// here is the REVERSE of commit order, so the naive last-write-wins replay
	// disagrees with the database. This is the hazard consumers must design around.
	value := map[string]string{idA: "A", idB: "B"}
	replayed := ""
	for _, id := range sender.ids {
		if v, ok := value[id]; ok {
			replayed = v
		}
	}
	if replayed == dbValue {
		t.Fatalf("a last-write-wins replay reached %q, matching the database — the begin/commit "+
			"order inversion this test documents no longer reproduces. If TiDB now allocates "+
			"start TSOs so that this cannot happen, the ordering caveats in "+
			"docs/design/outbox-sequenced-log.md and README.md can be relaxed; verify before "+
			"deleting this test", replayed)
	}
	t.Logf("documented hazard reproduced: delivery order %v (begin order) replays to %q while the "+
		"database holds %q", sender.ids, replayed, dbValue)
}
