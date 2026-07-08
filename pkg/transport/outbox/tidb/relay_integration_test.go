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
// outbox.IDGenerator is ReuseMetadataID (the outbox row's event_id equals the
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
	r := sequence.NewRelay("e2e", tidb.NewStoreDB(testDB), sender,
		sequence.WithBatchSize(10), sequence.WithSequenceBatchSize(1000))

	// Prime the group on the empty log: this is where latest-default
	// initializes the offset (to 0, since nothing is sequenced yet).
	if err := r.RunOnce(context.Background()); err != nil {
		t.Fatalf("priming RunOnce: %v", err)
	}
	if len(sender.ids) != 0 {
		t.Fatalf("priming delivered %d, want 0", len(sender.ids))
	}

	wantIDs := make([]string, 0, 50)
	for i := 0; i < 50; i++ {
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
	off, err := tidb.NewStore(testDB).Offset(context.Background(), "e2e")
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
	for i := 0; i < 5; i++ {
		wantIDs = append(wantIDs, publish(t, "s"))
	}

	sender := &recordingSender{}
	r := sequence.NewRelay("replay", tidb.NewStoreDB(testDB), sender,
		sequence.WithStartFromBeginning())

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

func TestLatePublishGetsHigherSeq(t *testing.T) {
	if testDB == nil {
		t.Skip("no TiDB")
	}
	truncate(t)
	publish(t, "early")

	st := tidb.NewStoreDB(testDB)
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

	var earlySeq, lateSeq int64
	if err := testDB.QueryRow(
		`SELECT seq FROM outbox WHERE JSON_UNQUOTE(JSON_EXTRACT(metadata, '$.Subject')) = 'early'`,
	).Scan(&earlySeq); err != nil {
		t.Fatal(err)
	}
	if err := testDB.QueryRow(
		`SELECT seq FROM outbox WHERE JSON_UNQUOTE(JSON_EXTRACT(metadata, '$.Subject')) = 'late'`,
	).Scan(&lateSeq); err != nil {
		t.Fatal(err)
	}
	if lateSeq <= earlySeq {
		t.Fatalf("late seq %d not > early seq %d", lateSeq, earlySeq)
	}
}
