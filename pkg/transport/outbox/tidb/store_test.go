package tidb_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/quarks-tech/protoevent-go/pkg/event"
	"github.com/quarks-tech/protoevent-go/pkg/transport/outbox"
	tidb "github.com/quarks-tech/protoevent-go/pkg/transport/outbox/tidb"
	"github.com/quarks-tech/protoevent-go/pkg/transport/outbox/tidb/tidbtest"
)

func TestNewStoreCompiles(t *testing.T) {
	// nil Runner is fine for a construction-only check; no query is issued.
	if tidb.NewStore(nil) == nil {
		t.Fatal("NewStore returned nil")
	}
}

func TestRelayStoreConstructs(t *testing.T) {
	if tidb.NewRelayStore(nil) == nil {
		t.Fatal("NewRelayStore returned nil")
	}
}

var testDB *sql.DB

func TestMain(m *testing.M) {
	inst, cleanup, err := tidbtest.Start(context.Background())
	if err != nil {
		if errors.Is(err, tidbtest.ErrDockerUnavailable) {
			fmt.Fprintf(os.Stderr, "skipping tidb integration tests: %v\n", err)
			os.Exit(0)
		}
		fmt.Fprintf(os.Stderr, "tidb integration setup: %v\n", err)
		os.Exit(1) // real harness bug, not a missing Docker
	}
	testDB = inst.DB
	code := m.Run()
	cleanup()
	os.Exit(code)
}

func truncate(t *testing.T) {
	t.Helper()
	for _, q := range []string{
		"DELETE FROM outbox", "DELETE FROM outbox_offsets", "DELETE FROM relay_lock",
		"UPDATE outbox_sequencer SET next_seq = 1 WHERE name = 'default'",
	} {
		if _, err := testDB.Exec(q); err != nil {
			t.Fatalf("reset (%s): %v", q, err)
		}
	}
}

func publish(t *testing.T, subject string) string {
	t.Helper()
	md := event.NewMetadata("books.created")
	md.ID = uuid.NewString()
	md.Source = "books-service"
	md.Subject = subject
	md.DataContentType = "application/proto"
	md.Time = time.Now().UTC()
	return publishMetadata(t, md, []byte("x"))
}

// publishMetadata publishes a caller-prepared *event.Metadata through the
// production Sender (so CreateTime is stamped like a real publish), within a
// transaction-scoped Runner. Returns the outbox row/event ID.
func publishMetadata(t *testing.T, md *event.Metadata, data []byte) string {
	t.Helper()
	tx, err := testDB.Begin()
	if err != nil {
		t.Fatal(err)
	}
	st := tidb.NewStore(tx)
	if err := outbox.NewSender(st).Send(context.Background(), md, data); err != nil {
		_ = tx.Rollback()
		t.Fatalf("publish: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	return md.ID
}

func TestPublishInsertsUnsequencedRow(t *testing.T) {
	if testDB == nil {
		t.Skip("no TiDB")
	}
	truncate(t)
	publish(t, "s1")

	var nullCount int
	if err := testDB.QueryRow("SELECT COUNT(*) FROM outbox WHERE seq IS NULL AND tx_start_ts > 0").Scan(&nullCount); err != nil {
		t.Fatal(err)
	}
	if nullCount != 1 {
		t.Fatalf("unsequenced rows = %d, want 1 (seq NULL, tx_start_ts set)", nullCount)
	}
}

func TestSequenceAssignsDenseContiguousSeq(t *testing.T) {
	if testDB == nil {
		t.Skip("no TiDB")
	}
	truncate(t)
	for range 5 {
		publish(t, "s")
	}
	st := tidb.NewRelayStore(testDB)
	n, err := st.SequenceMessages(context.Background(), 100)
	if err != nil {
		t.Fatalf("sequence: %v", err)
	}
	if n != 5 {
		t.Fatalf("sequenced %d, want 5", n)
	}
	rows, err := testDB.Query("SELECT seq FROM outbox ORDER BY seq")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var want int64 = 1
	for rows.Next() {
		var seq int64
		if err := rows.Scan(&seq); err != nil {
			t.Fatal(err)
		}
		if seq != want {
			t.Fatalf("seq = %d, want %d (dense contiguous)", seq, want)
		}
		want++
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if want != 6 {
		t.Fatalf("consumed %d rows, want 5", want-1)
	}
}

func TestConcurrentSequencersNoDuplicateNoGap(t *testing.T) {
	if testDB == nil {
		t.Skip("no TiDB")
	}
	truncate(t)
	for range 200 {
		publish(t, "s")
	}
	st := tidb.NewRelayStore(testDB)

	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			for {
				n, err := st.SequenceMessages(context.Background(), 20)
				if err != nil {
					t.Errorf("sequence: %v", err)
					return
				}
				if n == 0 {
					return
				}
			}
		})
	}
	wg.Wait()

	// Assert seq is exactly 1..200 with no dup and no gap.
	var count, minSeq, maxSeq, distinct int64
	if err := testDB.QueryRow("SELECT COUNT(*), MIN(seq), MAX(seq), COUNT(DISTINCT seq) FROM outbox").
		Scan(&count, &minSeq, &maxSeq, &distinct); err != nil {
		t.Fatal(err)
	}
	if count != 200 || distinct != 200 || minSeq != 1 || maxSeq != 200 {
		t.Fatalf("count=%d distinct=%d min=%d max=%d, want 200/200/1/200 (FOR UPDATE must serialize)",
			count, distinct, minSeq, maxSeq)
	}
}

func TestCommitOffsetIsMonotone(t *testing.T) {
	if testDB == nil {
		t.Skip("no TiDB")
	}
	truncate(t)
	st := tidb.NewRelayStore(testDB)
	ctx := context.Background()
	if err := st.CommitOffset(ctx, "c", 10); err != nil {
		t.Fatal(err)
	}
	if err := st.CommitOffset(ctx, "c", 5); err != nil { // must not rewind
		t.Fatal(err)
	}
	off, err := st.Offset(ctx, "c")
	if err != nil {
		t.Fatal(err)
	}
	if off != 10 {
		t.Fatalf("offset = %d, want 10 (GREATEST must not rewind)", off)
	}
}

func TestLeaderLockMutualExclusionAndRelease(t *testing.T) {
	if testDB == nil {
		t.Skip("no TiDB")
	}
	truncate(t)
	st := tidb.NewRelayStore(testDB)
	ctx := context.Background()

	okA, err := st.TryAcquireLeaderLock(ctx, "lock", "A", 30*time.Second)
	if err != nil || !okA {
		t.Fatalf("A acquire = %v, %v; want true", okA, err)
	}
	okB, err := st.TryAcquireLeaderLock(ctx, "lock", "B", 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if okB {
		t.Fatal("B acquired while A holds the lock")
	}
	if err := st.ReleaseLeaderLock(ctx, "lock", "A"); err != nil {
		t.Fatal(err)
	}
	okB2, err := st.TryAcquireLeaderLock(ctx, "lock", "B", 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if !okB2 {
		t.Fatal("B failed to acquire after A released")
	}
}

// TestMetadataFullFidelityRoundTrip proves the TiDB store now round-trips the
// FULL CloudEvents envelope through the metadata JSON column — Extensions and
// DataSchema in particular, which the old scalar-column schema dropped.
func TestMetadataFullFidelityRoundTrip(t *testing.T) {
	if testDB == nil {
		t.Skip("no TiDB")
	}
	truncate(t)

	md := event.NewMetadata("books.created")
	md.ID = uuid.NewString()
	md.SpecVersion = "1.0"
	md.Source = "books-service"
	md.Subject = "fidelity"
	md.DataContentType = "application/proto"
	md.Time = time.Now().UTC()
	md.Extensions = map[string]any{"partitionkey": "k1"}
	schema, err := url.Parse("https://schemas.example.com/books/created/v1")
	if err != nil {
		t.Fatal(err)
	}
	md.DataSchema = schema

	publishMetadata(t, md, []byte("payload"))

	st := tidb.NewRelayStore(testDB)
	if _, err := st.SequenceMessages(context.Background(), 100); err != nil {
		t.Fatalf("sequence: %v", err)
	}
	msgs, err := st.ListMessages(context.Background(), 0, 10)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("got %d messages, want 1", len(msgs))
	}
	got := msgs[0].Metadata

	if got.SpecVersion != "1.0" {
		t.Fatalf("SpecVersion = %q, want %q", got.SpecVersion, "1.0")
	}
	if got.Type != md.Type {
		t.Fatalf("Type = %q, want %q", got.Type, md.Type)
	}
	if got.Source != md.Source {
		t.Fatalf("Source = %q, want %q", got.Source, md.Source)
	}
	if got.Subject != md.Subject {
		t.Fatalf("Subject = %q, want %q", got.Subject, md.Subject)
	}
	if got.DataContentType != md.DataContentType {
		t.Fatalf("DataContentType = %q, want %q", got.DataContentType, md.DataContentType)
	}
	if v, ok := got.Extensions["partitionkey"]; !ok || v != "k1" {
		t.Fatalf("Extensions[partitionkey] = %v, ok=%v; want k1, true", v, ok)
	}
	if got.DataSchema == nil {
		t.Fatal("DataSchema is nil, want round-tripped URL")
	}
	if got.DataSchema.String() != schema.String() {
		t.Fatalf("DataSchema = %q, want %q", got.DataSchema.String(), schema.String())
	}
}
