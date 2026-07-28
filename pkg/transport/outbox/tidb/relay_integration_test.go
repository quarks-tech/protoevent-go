package tidb_test

import (
	"context"
	"reflect"
	"sync"
	"testing"

	"github.com/quarks-tech/protoevent-go/pkg/event"
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
