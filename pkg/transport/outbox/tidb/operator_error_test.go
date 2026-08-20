package tidb_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/quarks-tech/protoevent-go/pkg/event"
	"github.com/quarks-tech/protoevent-go/pkg/transport/outbox"
	"github.com/quarks-tech/protoevent-go/pkg/transport/outbox/relay"
	"github.com/quarks-tech/protoevent-go/pkg/transport/outbox/relay/sequence"
	"github.com/quarks-tech/protoevent-go/pkg/transport/outbox/tidb"
)

// TestRepublishingAnEventIDIsReportedAsAlreadyPublished covers the retried-transaction
// case (S15).
//
// Retrying a business transaction is routine on TiDB: a write conflict, a deadlock, or a
// commit whose outcome was AMBIGUOUS because the connection dropped after the primary
// key was written. The retry re-runs the handler, which re-publishes the same event under
// the same ID — the default row-ID generator reuses Metadata.ID — and uk_outbox_event
// rejects it.
//
// Reported as a bare 1062 that aborts the caller's transaction, that fails the user's
// request permanently, on every attempt, for an event that is already durable and will
// be delivered. The sentinel lets the caller commit instead.
func TestRepublishingAnEventIDIsReportedAsAlreadyPublished(t *testing.T) {
	if testDB == nil {
		t.Skip("no TiDB")
	}
	truncate(t)

	md := newTestMetadata("retried")

	// The first attempt commits, exactly as an ambiguous commit does.
	if err := publishMetadataErr(md, []byte("x")); err != nil {
		t.Fatalf("first publish: %v", err)
	}

	// The retry re-publishes the same event.
	err := publishMetadataErr(md, []byte("x"))
	if err == nil {
		t.Fatal("re-publishing an existing event ID succeeded: uk_outbox_event should reject it")
	}
	if !errors.Is(err, outbox.ErrAlreadyPublished) {
		t.Fatalf("error does not wrap outbox.ErrAlreadyPublished, so a retried transaction cannot "+
			"tell an already-durable event from a write failure: %v", err)
	}
	if !strings.Contains(err.Error(), md.ID) {
		t.Errorf("error does not name the event ID: %v", err)
	}

	// Exactly one row, and it is still the original.
	var n int
	if err := testDB.QueryRowContext(context.Background(),
		"SELECT COUNT(*) FROM outbox_messages").Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Fatalf("outbox holds %d rows, want 1", n)
	}
}

// TestSequencerCounterRowMissingIsNamed covers the second operator-error case.
//
// The counter row is seeded once by the migration, so its absence is permanent: every
// pass fails, nothing is assigned a seq, and the relay delivers NOTHING for the lifetime
// of the deployment. The bare error said "sql: no rows in result set", naming
// database/sql rather than the row that is gone or how to restore it.
//
// The package's own truncate() helper UPDATEs this row instead of deleting it, which is
// why every other test in the package guarantees it exists.
func TestSequencerCounterRowMissingIsNamed(t *testing.T) {
	if testDB == nil {
		t.Skip("no TiDB")
	}
	truncate(t)
	ctx := context.Background()

	publish(t, "orphan")

	if _, err := testDB.ExecContext(ctx, "DELETE FROM outbox_sequencers"); err != nil {
		t.Fatalf("delete counter row: %v", err)
	}

	_, err := tidb.NewRelayStore(testDB).SequenceMessages(ctx, 100)
	if err == nil {
		t.Fatal("sequencing succeeded with no counter row")
	}

	for _, want := range []string{"outbox_sequencers", "name='default'", "INSERT INTO"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %q, so an operator cannot act on it: %v", want, err)
		}
	}

	// The remedy the error prescribes must actually work, including the MAX(seq) part
	// that keeps it from colliding with uk_outbox_seq.
	if _, err := testDB.ExecContext(ctx,
		"INSERT INTO outbox_sequencers (name, next_seq) SELECT 'default', COALESCE(MAX(seq), 0) + 1 FROM outbox_messages",
	); err != nil {
		t.Fatalf("the prescribed remedy failed: %v", err)
	}
	n, err := tidb.NewRelayStore(testDB).SequenceMessages(ctx, 100)
	if err != nil {
		t.Fatalf("sequencing after the remedy: %v", err)
	}
	if n != 1 {
		t.Fatalf("sequenced %d rows after the remedy, want 1", n)
	}
}

// TestTwoDeploymentsSharingARelayNameSplitTheLog documents the third case, which is NOT
// currently guarded.
//
// outbox_offsets.name and relay_locks.name are both primary keys, and the leader lock
// name defaults to the group name. So two INDEPENDENT deployments that happen to reuse a
// relay name — a copied wiring snippet — contend as if they were replicas of one relay:
// exactly one leads at a time, drains, and advances the SHARED watermark past events the
// other never saw. Each destination receives an arbitrary subset, both report healthy
// OnDrained, and the sweep eventually deletes the rows as fully consumed.
//
// This test pins the CURRENT behavior so the loss is visible in the suite rather than
// discovered in production. Closing it properly needs an identity the library does not
// have today — the group name is the only thing distinguishing consumers, and two
// deployments asserting the same name are indistinguishable from two replicas of one.
func TestTwoDeploymentsSharingARelayNameSplitTheLog(t *testing.T) {
	if testDB == nil {
		t.Skip("no TiDB")
	}
	truncate(t)
	ctx := context.Background()

	const total = 20
	for i := range total {
		publish(t, "shared-"+string(rune('a'+i)))
	}
	if _, err := tidb.NewRelayStore(testDB).SequenceMessages(ctx, 100); err != nil {
		t.Fatalf("sequence: %v", err)
	}

	// Two deployments, same group name, DIFFERENT destinations.
	type recorder struct {
		mu  sync.Mutex
		ids []string
	}
	recA, recB := &recorder{}, &recorder{}
	senderFor := func(r *recorder) senderFn {
		return func(_ context.Context, md *event.Metadata, _ []byte) error {
			r.mu.Lock()
			defer r.mu.Unlock()
			r.ids = append(r.ids, md.ID)

			return nil
		}
	}

	newRelay := func(r *recorder) *sequence.Relay {
		rel, err := sequence.NewRelay("shared", tidb.NewRelayStore(testDB), senderFor(r),
			sequence.WithPollInterval(20*time.Millisecond),
			sequence.WithBatchSize(5),
			sequence.WithStartFromBeginning(),
			sequence.WithoutRetention(),
		)
		if err != nil {
			t.Fatalf("NewRelay: %v", err)
		}

		return rel
	}

	relA, relB := newRelay(recA), newRelay(recB)

	runCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	var wg sync.WaitGroup
	for _, rel := range []*sequence.Relay{relA, relB} {
		wg.Go(func() {
			for runCtx.Err() == nil {
				if err := rel.RunOnce(runCtx); err != nil {
					return
				}
				// Give the other instance a chance to win the lock.
				time.Sleep(10 * time.Millisecond)
			}
		})
	}

	// Drain until the shared watermark covers the whole log.
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		if offsetOrZero(t, "shared") >= total {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	cancel()
	wg.Wait()

	recA.mu.Lock()
	gotA := len(recA.ids)
	recA.mu.Unlock()
	recB.mu.Lock()
	gotB := len(recB.ids)
	recB.mu.Unlock()

	t.Logf("destination A received %d/%d events, destination B received %d/%d; shared watermark=%d",
		gotA, total, gotB, total, offsetOrZero(t, "shared"))

	if gotA+gotB == 0 {
		t.Fatal("neither destination received anything; the scenario did not run")
	}

	// The invariant that makes this a data-loss bug rather than a scheduling detail:
	// ONE watermark covers both deployments, so each event is delivered to whichever
	// instance held the lock and to no other. The two destinations therefore share the
	// log between them instead of each receiving it — the total across both lands near
	// `total`, not near 2*total, however the lock happened to alternate.
	//
	// Asserted this way rather than "A got a subset and so did B", because the split is
	// timing-dependent: one instance can hold the lock for the whole run and starve the
	// other completely, which is what makes the failure so quiet.
	if gotA+gotB >= 2*total {
		t.Errorf("each destination received the whole log (%d and %d of %d): if the library now "+
			"guards shared names, this documentation test is stale and should be replaced by the "+
			"real assertion", gotA, gotB, total)
	} else {
		t.Logf("CONFIRMED: sharing the relay name %q split the log between two destinations "+
			"(%d and %d of %d delivered, %d in total). Every health signal stayed green and "+
			"nothing in the library detects this today.", "shared", gotA, gotB, total, gotA+gotB)
	}
}

// senderFn adapts a func to eventbus.Sender for the tests in this file.
type senderFn func(context.Context, *event.Metadata, []byte) error

func (f senderFn) Send(ctx context.Context, md *event.Metadata, d []byte) error {
	return f(ctx, md, d)
}

// TestSweepDrainsABacklogLargerThanOnePassCap is the store-backed half of the sweep
// throughput fix.
//
// The relay-level test proves the retention interval is no longer consumed when pages
// come back full. This one proves the consequence against real TiDB: a backlog larger
// than maxPagesPerTick * batch is actually drained by successive passes, with no
// wall-clock interval elapsing between them.
//
// Before the fix the arithmetic was maxPagesPerTick(64) * RetentionSweepBatch(1000) per
// RetentionSweepInterval(1h) = 17.8 rows/s, so any outbox publishing faster grew forever.
func TestSweepDrainsABacklogLargerThanOnePassCap(t *testing.T) {
	if testDB == nil {
		t.Skip("no TiDB")
	}
	truncate(t)
	ctx := context.Background()

	// Deliberately more than one pass can delete: the sweep loop caps at 64 pages, so a
	// batch of 100 bounds a pass at 6,400 rows.
	const (
		backlog = 8000
		batch   = 100
	)

	// Seed sequenced, long-expired rows in one statement.
	var b strings.Builder
	b.WriteString("INSERT INTO outbox_messages (seq, tx_start_ts, event_id, metadata, data, create_time, occur_time) VALUES ")
	for i := range backlog {
		if i > 0 {
			b.WriteString(",")
		}
		fmt.Fprintf(&b, "(%d, %d, RANDOM_BYTES(16), '{\"Time\":\"2026-01-01T00:00:00Z\"}', 'x', "+
			"NOW(6) - INTERVAL 30 DAY, NOW(6))", i+1, 1000+i)
	}
	if _, err := testDB.ExecContext(ctx, b.String()); err != nil {
		t.Fatalf("seed backlog: %v", err)
	}

	// Every row is fully consumed, so only the create_time cutoff decides deletion.
	st := tidb.NewRelayStore(testDB, tidb.WithRetentionWindow(24*time.Hour))
	if err := st.CommitOffset(ctx, "g", backlog); err != nil {
		t.Fatalf("commit offset: %v", err)
	}

	var swept []int
	r, err := sequence.NewRelay("g", st, senderFn(
		func(context.Context, *event.Metadata, []byte) error { return nil }),
		sequence.WithPollInterval(20*time.Millisecond),
		// An hour-long idle cadence, as the defaults have.
		sequence.WithRetention(time.Hour, batch),
		sequence.WithObserver(relay.Observer{OnSwept: func(_ string, n int) { swept = append(swept, n) }}),
	)
	if err != nil {
		t.Fatalf("NewRelay: %v", err)
	}

	// Successive passes with no wall-clock interval between them.
	for pass := range 5 {
		if err := r.RunOnce(ctx); err != nil {
			t.Fatalf("pass %d: %v", pass, err)
		}
		if remainingRows(t) == 0 {
			break
		}
	}

	if left := remainingRows(t); left != 0 {
		total := 0
		for _, n := range swept {
			total += n
		}
		t.Fatalf("%d of %d rows survived: the sweep sees %d deletable rows it is not deleting, "+
			"because hitting the page cap consumed the retention interval (swept %d in %d pages)",
			left, backlog, left, total, len(swept))
	}
}

// remainingRows counts what is left in the outbox.
func remainingRows(t *testing.T) int {
	t.Helper()

	var n int
	if err := testDB.QueryRowContext(context.Background(),
		"SELECT COUNT(*) FROM outbox_messages").Scan(&n); err != nil {
		t.Fatalf("count rows: %v", err)
	}

	return n
}
