package tidb_test

import (
	"context"
	"database/sql"
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

func TestStoreDBConstructs(t *testing.T) {
	if tidb.NewStoreDB(nil) == nil {
		t.Fatal("NewStoreDB returned nil")
	}
}

var testDB *sql.DB

func TestMain(m *testing.M) {
	inst, cleanup, err := tidbtest.Start(context.Background())
	if err != nil {
		// Docker unavailable: skip the whole integration suite.
		os.Exit(0)
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

func publish(t *testing.T, subject string) {
	t.Helper()
	tx, err := testDB.Begin()
	if err != nil {
		t.Fatal(err)
	}
	st := tidb.NewStore(tx)
	md := event.NewMetadata("books.created")
	md.ID = uuid.NewString()
	md.Source = "books-service"
	md.Subject = subject
	md.DataContentType = "application/proto"
	md.Time = time.Now().UTC()
	if err := st.CreateOutboxMessage(context.Background(), &outbox.Message{ID: md.ID, Metadata: md, Data: []byte("x")}); err != nil {
		_ = tx.Rollback()
		t.Fatalf("publish: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
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
	for i := 0; i < 5; i++ {
		publish(t, "s")
	}
	st := tidb.NewStoreDB(testDB)
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
	for i := 0; i < 200; i++ {
		publish(t, "s")
	}
	st := tidb.NewStoreDB(testDB)

	var wg sync.WaitGroup
	for g := 0; g < 4; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
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
		}()
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
	st := tidb.NewStore(testDB)
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
	st := tidb.NewStore(testDB)
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
