package tidb_test

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/quarks-tech/protoevent-go/pkg/transport/outbox"
	"github.com/quarks-tech/protoevent-go/pkg/transport/outbox/tidb"
)

// seqOf reports the seq assigned to one outbox row AS THE RELAY SEES IT, and whether
// there is one.
//
// A row published by a transaction that has not committed is not visible on this
// connection at all, so the query returns no rows — which is the same answer the
// sequencer's own page read gets, and the reason "no seq yet" and "not visible yet"
// are deliberately one condition here rather than two.
func seqOf(t *testing.T, eventID string) (int64, bool) {
	t.Helper()

	// event_id is a BINARY(16) column, so it must be matched with the UUID's bytes;
	// a string comparison silently matches nothing.
	u, perr := uuid.Parse(eventID)
	if perr != nil {
		t.Fatalf("event id %q is not a UUID: %v", eventID, perr)
	}

	var seq sql.NullInt64
	err := testDB.QueryRowContext(context.Background(),
		"SELECT seq FROM outbox_messages WHERE event_id = ?", u[:]).Scan(&seq)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false
	}
	if err != nil {
		t.Fatalf("read seq of %s: %v", eventID, err)
	}

	return seq.Int64, seq.Valid
}

// assertDenseSeq fails unless the sequenced rows are exactly 1..want with no gap and
// no duplicate, and nothing is left unsequenced. This is the log's core invariant: a
// gap means a consumer's `seq > ?` cursor steps over an event forever.
func assertDenseSeq(t *testing.T, want int64) {
	t.Helper()

	var (
		total, distinct, unsequenced int64
		minSeq, maxSeq               sql.NullInt64
	)
	row := testDB.QueryRowContext(context.Background(), `
SELECT COUNT(seq), COUNT(DISTINCT seq), SUM(seq IS NULL), MIN(seq), MAX(seq)
FROM outbox_messages`)
	if err := row.Scan(&total, &distinct, &unsequenced, &minSeq, &maxSeq); err != nil {
		t.Fatalf("read seq shape: %v", err)
	}

	if unsequenced != 0 {
		t.Errorf("%d rows are still unsequenced: they will never be listed by seq > ?", unsequenced)
	}
	if total != want || distinct != want {
		t.Errorf("sequenced rows = %d (%d distinct), want %d of each", total, distinct, want)
	}
	if minSeq.Int64 != 1 || maxSeq.Int64 != want {
		t.Errorf("seq range = [%d, %d], want [1, %d]", minSeq.Int64, maxSeq.Int64, want)
	}
}

// TestOpenTransactionStraddlingASequencerPassGetsAHigherSeq is the executable form of
// the design's exactness claim — race-matrix row one: a publish that is still
// uncommitted when a sequencer pass runs is invisible to that pass, and is therefore
// assigned a seq ABOVE the offset the pass committed. "Gap structurally impossible."
//
// If it did not hold, the row would get a seq BELOW the committed offset, would never
// be returned by `seq > ?`, and would be swept as consumed: permanent, total, silent
// loss with no error anywhere. It is the highest-consequence invariant in the module,
// and no fake can cover it — it is a property of TiDB's MVCC visibility, not of this
// code.
//
// The nearest existing tests both commit their transactions BEFORE the pass they
// measure: TestLatePublishGetsHigherSeq publishes-commits-sequences twice over, and
// TestSameAggregateSeqFollowsBeginOrderNotCommitOrder holds two transactions open but
// sequences only after both have committed. Nothing was ever in flight ACROSS a pass.
//
// Driven through RelayStore directly rather than a relay: the invariant is about what
// one sequencer pass can see, and the store-level calls make the interleaving exact
// instead of depending on tick timing.
func TestOpenTransactionStraddlingASequencerPassGetsAHigherSeq(t *testing.T) {
	if testDB == nil {
		t.Skip("no TiDB")
	}

	for _, tc := range []struct {
		name    string
		txnMode string
	}{
		{"pessimistic publisher", "pessimistic"},
		// Optimistic mode is a different visibility and conflict regime, and many
		// MySQL-migrated clusters run it globally. The invariant must hold in both.
		{"optimistic publisher", "optimistic"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			truncate(t)
			ctx := t.Context()

			// A separate *sql.DB, not a connection borrowed from testDB.
			//
			// sql.Conn.Close() RETURNS the connection to its pool with session state
			// intact, so setting tidb_txn_mode on a borrowed conn leaks optimistic mode
			// into every later test that happens to draw it — which reliably breaks
			// TestConcurrentSequencersNoDuplicateNoGap with "Error 9007 Write conflict
			// ... reason=Optimistic". Closing a whole *sql.DB discards its connections
			// instead, so the mode cannot escape this subtest.
			// The "mysql" driver is already registered: tidbtest links it into this
			// test binary.
			pub, err := sql.Open("mysql", testDSN)
			if err != nil {
				t.Fatalf("open publisher db: %v", err)
			}
			defer func() { _ = pub.Close() }()
			// One connection, so the SET below governs every statement this pool runs.
			pub.SetMaxOpenConns(1)

			if _, err := pub.ExecContext(ctx, "SET SESSION tidb_txn_mode = ?", tc.txnMode); err != nil {
				t.Fatalf("set txn mode %q: %v", tc.txnMode, err)
			}

			var mode string
			if err := pub.QueryRowContext(ctx, "SELECT @@tidb_txn_mode").Scan(&mode); err != nil {
				t.Fatalf("read back txn mode: %v", err)
			}
			if mode != tc.txnMode {
				t.Fatalf("tidb_txn_mode = %q, want %q: the subtest is not exercising the mode it names",
					mode, tc.txnMode)
			}

			txSlow, err := pub.BeginTx(ctx, nil)
			if err != nil {
				t.Fatalf("begin slow tx: %v", err)
			}
			defer func() { _ = txSlow.Rollback() }()

			// Materialize the start TSO before anything else exists, so the slow
			// transaction is genuinely the OLDEST writer — the case that would
			// produce a below-the-offset seq if visibility worked differently.
			var startTS uint64
			if err := txSlow.QueryRowContext(ctx, "SELECT @@tidb_current_ts").Scan(&startTS); err != nil {
				t.Fatalf("materialize start ts: %v", err)
			}
			if startTS == 0 {
				t.Fatal("start TSO is 0: the publish would be stamped with a zero tx_start_ts")
			}

			slowMD := newTestMetadata("slow")
			if err := outbox.NewSender(tidb.NewStore(txSlow)).Send(ctx, slowMD, []byte("slow")); err != nil {
				t.Fatalf("publish in the slow tx: %v", err)
			}

			// Ten ordinary publishes that commit while the slow one is still open.
			const fast = 10
			for i := range fast {
				publish(t, "fast-"+string(rune('a'+i)))
			}

			st := tidb.NewRelayStore(testDB)

			// Pass one: the slow row is uncommitted and must be invisible.
			n, err := st.SequenceMessages(ctx, 100)
			if err != nil {
				t.Fatalf("sequence pass 1: %v", err)
			}
			if n != fast {
				t.Fatalf("pass 1 sequenced %d rows, want %d: an uncommitted publish was visible "+
					"to the sequencer", n, fast)
			}

			msgs, err := st.ListMessages(ctx, 0, 100)
			if err != nil {
				t.Fatalf("list pass 1: %v", err)
			}
			if len(msgs) != fast {
				t.Fatalf("pass 1 listed %d messages, want %d", len(msgs), fast)
			}
			if err := st.CommitOffset(ctx, "g", msgs[len(msgs)-1].Seq); err != nil {
				t.Fatalf("commit offset: %v", err)
			}
			committed := msgs[len(msgs)-1].Seq

			if _, ok := seqOf(t, slowMD.ID); ok {
				t.Fatal("the uncommitted row already carries a seq: the sequencer assigned one to a " +
					"row whose transaction had not committed")
			}

			// The slow transaction finally commits — long after the pass that could
			// have sequenced it, and after the offset moved past everything else.
			if err := txSlow.Commit(); err != nil {
				t.Fatalf("commit slow tx: %v", err)
			}

			// Pass two: now it is visible, and its seq must be ABOVE the committed
			// offset or it is unreachable forever.
			n, err = st.SequenceMessages(ctx, 100)
			if err != nil {
				t.Fatalf("sequence pass 2: %v", err)
			}
			if n != 1 {
				t.Fatalf("pass 2 sequenced %d rows, want 1", n)
			}

			slowSeq, ok := seqOf(t, slowMD.ID)
			if !ok {
				t.Fatal("the committed row was still not assigned a seq")
			}
			if slowSeq <= committed {
				t.Fatalf("the late publish got seq %d, at or below the committed offset %d: it can "+
					"never be returned by seq > %d and will be swept as consumed — silent loss",
					slowSeq, committed, committed)
			}

			// And it is actually delivered by the cursor the relay uses.
			after, err := st.ListMessages(ctx, committed, 100)
			if err != nil {
				t.Fatalf("list pass 2: %v", err)
			}
			if len(after) != 1 || after[0].Metadata.ID != slowMD.ID {
				t.Fatalf("listing after offset %d returned %d messages, want exactly the late publish",
					committed, len(after))
			}

			assertDenseSeq(t, fast+1)
		})
	}
}

// TestEventsInOneTransactionKeepEmissionOrder pins the tiebreak that decides order
// WITHIN a business transaction.
//
// Events published in one transaction share a tx_start_ts, so the sequencer's window
// (ORDER BY tx_start_ts, id) resolves them purely on the AUTO_INCREMENT id. The schema
// comment says so and forbids row-id sharding because of it — while the same migration
// documents an AUTO_RANDOM escape hatch for the tail-region hotspot, and the design doc
// states the opposite of the shipped code, calling id a physical key never used for
// ordering.
//
// Nothing tested the id half: every publish helper in this package sends exactly one
// event per transaction. So a future migration that takes the documented escape hatch
// without adding the emit-order column would silently randomize intra-transaction
// order — consumers receiving PaymentRequested before OrderCreated — with no test
// failing.
func TestEventsInOneTransactionKeepEmissionOrder(t *testing.T) {
	if testDB == nil {
		t.Skip("no TiDB")
	}
	truncate(t)

	ctx := t.Context()

	tx, err := testDB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback() }()

	// A causal chain: each event only makes sense after the one before it.
	subjects := []string{"OrderCreated", "ItemsReserved", "PaymentRequested", "InvoiceIssued", "OrderConfirmed"}
	want := make([]string, 0, len(subjects))
	sender := outbox.NewSender(tidb.NewStore(tx))
	for _, s := range subjects {
		md := newTestMetadata(s)
		if err := sender.Send(ctx, md, []byte(s)); err != nil {
			t.Fatalf("publish %s: %v", s, err)
		}
		want = append(want, md.ID)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	// Guard the premise the way TestSameAggregateSeq... guards its own: if these rows
	// did NOT share a start TSO, the assertion below would be testing tx_start_ts
	// ordering and would pass for the wrong reason.
	var distinctTS int
	if err := testDB.QueryRowContext(ctx,
		"SELECT COUNT(DISTINCT tx_start_ts) FROM outbox_messages").Scan(&distinctTS); err != nil {
		t.Fatalf("count distinct tx_start_ts: %v", err)
	}
	if distinctTS != 1 {
		t.Fatalf("rows span %d distinct tx_start_ts values, want 1: this test can only exercise the "+
			"id tiebreak when every row shares one start TSO", distinctTS)
	}

	st := tidb.NewRelayStore(testDB)
	if _, err := st.SequenceMessages(ctx, 100); err != nil {
		t.Fatalf("sequence: %v", err)
	}

	msgs, err := st.ListMessages(ctx, 0, 100)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(msgs) != len(want) {
		t.Fatalf("listed %d messages, want %d", len(msgs), len(want))
	}

	for i, m := range msgs {
		if m.Metadata.ID != want[i] {
			gotSubjects := make([]string, 0, len(msgs))
			for _, mm := range msgs {
				gotSubjects = append(gotSubjects, mm.Metadata.Subject)
			}
			t.Fatalf("delivery order = %v, want %v: events published in one transaction were "+
				"reordered, so a consumer sees an effect before its cause", gotSubjects, subjects)
		}
		if m.Seq != int64(i+1) {
			t.Errorf("message %d has seq %d, want %d: intra-transaction seq is not contiguous",
				i, m.Seq, i+1)
		}
	}
}

// TestConcurrentTransactionsAreNotInterleaved pins the other half of the same
// tiebreak: two transactions publishing at the same time must come out as two
// CONTIGUOUS runs, not shuffled into each other.
//
// AUTO_INCREMENT ids are allocated as each row is inserted, so two interleaved
// publishers necessarily interleave their ids. Grouping therefore comes from
// tx_start_ts being the primary sort key, with id only breaking ties inside one
// transaction — and if that ordering were ever reversed, each transaction's events
// would be scattered through the other's while every single-event test still passed.
func TestConcurrentTransactionsAreNotInterleaved(t *testing.T) {
	if testDB == nil {
		t.Skip("no TiDB")
	}
	truncate(t)

	ctx := t.Context()

	txA, err := testDB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin A: %v", err)
	}
	defer func() { _ = txA.Rollback() }()
	txB, err := testDB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin B: %v", err)
	}
	defer func() { _ = txB.Rollback() }()

	senderA := outbox.NewSender(tidb.NewStore(txA))
	senderB := outbox.NewSender(tidb.NewStore(txB))

	// Interleave the INSERTS, so the id sequence alternates between the two
	// transactions and only tx_start_ts can group them.
	idsA := make([]string, 0, 3)
	idsB := make([]string, 0, 3)
	for i := range 3 {
		mdA := newTestMetadata("A" + string(rune('0'+i)))
		if err := senderA.Send(ctx, mdA, []byte("a")); err != nil {
			t.Fatalf("publish A%d: %v", i, err)
		}
		idsA = append(idsA, mdA.ID)

		mdB := newTestMetadata("B" + string(rune('0'+i)))
		if err := senderB.Send(ctx, mdB, []byte("b")); err != nil {
			t.Fatalf("publish B%d: %v", i, err)
		}
		idsB = append(idsB, mdB.ID)
	}

	if err := txA.Commit(); err != nil {
		t.Fatalf("commit A: %v", err)
	}
	if err := txB.Commit(); err != nil {
		t.Fatalf("commit B: %v", err)
	}

	st := tidb.NewRelayStore(testDB)
	if _, err := st.SequenceMessages(ctx, 100); err != nil {
		t.Fatalf("sequence: %v", err)
	}
	msgs, err := st.ListMessages(ctx, 0, 100)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(msgs) != 6 {
		t.Fatalf("listed %d messages, want 6", len(msgs))
	}

	// Which transaction each delivery came from, in delivery order.
	inA := make(map[string]bool, len(idsA))
	for _, id := range idsA {
		inA[id] = true
	}
	groups := make([]string, 0, len(msgs))
	for _, m := range msgs {
		if inA[m.Metadata.ID] {
			groups = append(groups, "A")
		} else {
			groups = append(groups, "B")
		}
	}

	// Exactly one transition: AAABBB or BBBAAA.
	transitions := 0
	for i := 1; i < len(groups); i++ {
		if groups[i] != groups[i-1] {
			transitions++
		}
	}
	if transitions != 1 {
		t.Fatalf("delivery grouping = %v (%d transitions), want two contiguous runs: concurrent "+
			"transactions were interleaved, so each one's events are scattered through the other's",
			groups, transitions)
	}

	// And within each run, emission order held.
	var seenA, seenB []string
	for _, m := range msgs {
		if inA[m.Metadata.ID] {
			seenA = append(seenA, m.Metadata.ID)
		} else {
			seenB = append(seenB, m.Metadata.ID)
		}
	}
	for i := range idsA {
		if seenA[i] != idsA[i] {
			t.Fatalf("transaction A delivered out of emission order at index %d", i)
		}
		if seenB[i] != idsB[i] {
			t.Fatalf("transaction B delivered out of emission order at index %d", i)
		}
	}

	assertDenseSeq(t, 6)
}
