package tidb_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/quarks-tech/protoevent-go/pkg/event"
	"github.com/quarks-tech/protoevent-go/pkg/transport/outbox"
	"github.com/quarks-tech/protoevent-go/pkg/transport/outbox/relay/sequence"
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

// TestCreateOutboxMessageRejectsNilMetadata proves the nil-Metadata guard
// fires before any SQL is issued: a nil Runner (via NewStore(nil)) would
// panic on md.Time.UTC() if the guard were missing, and would also panic on
// the first ExecContext call if the guard didn't run before it.
func TestCreateOutboxMessageRejectsNilMetadata(t *testing.T) {
	st := tidb.NewStore(nil)
	err := st.CreateOutboxMessage(context.Background(), &outbox.Message{ID: uuid.NewString()})
	if err == nil {
		t.Fatal("expected error for nil Metadata, got nil")
	}
}

// TestCreateOutboxMessageRejectsZeroTime proves the zero-Time guard fires
// before any SQL is issued (the nil Runner would panic on ExecContext if it
// didn't): a zero Metadata.Time would be sent as 0001-01-01, below the
// DATETIME(6) minimum, and surface only as an opaque driver error.
func TestCreateOutboxMessageRejectsZeroTime(t *testing.T) {
	st := tidb.NewStore(nil)
	md := event.NewMetadata("books.created") // Time left at its zero value
	md.ID = uuid.NewString()
	err := st.CreateOutboxMessage(context.Background(), &outbox.Message{ID: md.ID, Metadata: md})
	if err == nil || !strings.Contains(err.Error(), "time is zero") {
		t.Fatalf("err = %v, want descriptive zero-time error", err)
	}
}

var testDB *sql.DB

func TestMain(m *testing.M) {
	inst, cleanup, err := tidbtest.Start(context.Background())
	if err != nil {
		if errors.Is(err, tidbtest.ErrDockerUnavailable) {
			// In CI a broken Docker daemon must fail the module loudly — an
			// exit 0 here would silently pass the whole integration suite.
			if os.Getenv("CI") != "" {
				log.Fatalf("tidb integration tests require Docker in CI: %v", err)
			}
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

// truncate and publish/publishMetadata below take testing.TB (rather than
// *testing.T) so both tests and benchmarks in this package can share them.
func truncate(tb testing.TB) {
	tb.Helper()
	for _, q := range []string{
		"DELETE FROM outbox_messages", "DELETE FROM outbox_offsets", "DELETE FROM relay_locks",
		"UPDATE outbox_sequencers SET next_seq = 1 WHERE name = 'default'",
	} {
		if _, err := testDB.ExecContext(context.Background(), q); err != nil {
			tb.Fatalf("reset (%s): %v", q, err)
		}
	}
}

// newTestMetadata builds a fully populated publishable metadata envelope.
func newTestMetadata(subject string) *event.Metadata {
	md := event.NewMetadata("books.created")
	md.ID = uuid.NewString()
	md.Source = "books-service"
	md.Subject = subject
	md.DataContentType = "application/proto"
	md.Time = time.Now().UTC()
	return md
}

func publish(tb testing.TB, subject string) string {
	tb.Helper()
	return publishMetadata(tb, newTestMetadata(subject), []byte("x"))
}

// publishMetadata publishes a caller-prepared *event.Metadata through the
// production Sender (so CreateTime is stamped like a real publish), within a
// transaction-scoped Runner. Returns the outbox row/event ID.
func publishMetadata(tb testing.TB, md *event.Metadata, data []byte) string {
	tb.Helper()
	if err := publishMetadataErr(md, data); err != nil {
		tb.Fatalf("publish: %v", err)
	}
	return md.ID
}

// publishMetadataErr is the error-returning core of publishMetadata. It is
// safe to call from b.RunParallel goroutines, where testing.TB's Fatal/FailNow
// must not be used (FailNow must run on the test's own goroutine).
func publishMetadataErr(md *event.Metadata, data []byte) error {
	tx, err := testDB.BeginTx(context.Background(), nil)
	if err != nil {
		return err
	}
	st := tidb.NewStore(tx)
	if err := outbox.NewSender(st).Send(context.Background(), md, data); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

func TestPublishInsertsUnsequencedRow(t *testing.T) {
	if testDB == nil {
		t.Skip("no TiDB")
	}
	truncate(t)
	publish(t, "s1")

	var nullCount int
	if err := testDB.QueryRowContext(context.Background(), "SELECT COUNT(*) FROM outbox_messages WHERE seq IS NULL AND tx_start_ts > 0").Scan(&nullCount); err != nil {
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
	rows, err := testDB.QueryContext(context.Background(), "SELECT seq FROM outbox_messages ORDER BY seq")
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
	if err := testDB.QueryRowContext(context.Background(), "SELECT COUNT(*), MIN(seq), MAX(seq), COUNT(DISTINCT seq) FROM outbox_messages").
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

// TestLeaderLockExpiredLeaseTakeover proves a standby takes over once the
// lease expires — the `expire_time < NOW(6)` branch of TryAcquireLeaderLock's
// upsert, which the restart test bypasses via a manual DELETE. Expiry is
// decided by the SERVER clock (NOW(6)), the same clock that stamped the
// deadline, so the takeover works regardless of skew between relay instances'
// local clocks.
func TestLeaderLockExpiredLeaseTakeover(t *testing.T) {
	if testDB == nil {
		t.Skip("no TiDB")
	}
	truncate(t)
	st := tidb.NewRelayStore(testDB)
	ctx := context.Background()

	const shortTTL = 300 * time.Millisecond
	okA, err := st.TryAcquireLeaderLock(ctx, "lock", "A", shortTTL)
	if err != nil || !okA {
		t.Fatalf("A acquire = %v, %v; want true", okA, err)
	}
	// B is denied while A's lease is live.
	okB, err := st.TryAcquireLeaderLock(ctx, "lock", "B", 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if okB {
		t.Fatal("B acquired while A's lease is still live")
	}
	// Wait out A's lease with a generous margin (3x TTL), then B must win.
	time.Sleep(3 * shortTTL)
	okB2, err := st.TryAcquireLeaderLock(ctx, "lock", "B", 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if !okB2 {
		t.Fatal("B failed to take over an expired lease")
	}
	// And A, whose lease B replaced, must now be denied.
	okA2, err := st.TryAcquireLeaderLock(ctx, "lock", "A", 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if okA2 {
		t.Fatal("A re-acquired while B holds a live lease")
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

// TestCreateOutboxMessageRejectsAutocommit proves an autocommit publish fails
// loudly with a descriptive error instead of writing a row: on an autocommit
// connection @@tidb_current_ts is 0 (verified against live TiDB), and a
// tx_start_ts of 0 would silently sort the row before every transactional row
// in a sequencer batch.
func TestCreateOutboxMessageRejectsAutocommit(t *testing.T) {
	if testDB == nil {
		t.Skip("no TiDB")
	}
	truncate(t)

	md := newTestMetadata("autocommit")
	st := tidb.NewStore(testDB) // pool, not a transaction: autocommit
	err := st.CreateOutboxMessage(context.Background(), &outbox.Message{
		ID:         md.ID,
		Metadata:   md,
		Data:       []byte("x"),
		CreateTime: time.Now().UTC(),
	})
	if err == nil || !strings.Contains(err.Error(), "must run inside a transaction") {
		t.Fatalf("err = %v, want must-run-inside-a-transaction error", err)
	}

	var count int
	if err := testDB.QueryRowContext(context.Background(), "SELECT COUNT(*) FROM outbox_messages").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("outbox rows = %d, want 0 (autocommit publish must not write)", count)
	}

	// A transactional publish on the same store code path is unaffected.
	publish(t, "transactional")
}

// TestListMessagesPoisonRowReturnsPrefixAndDecodeError pins the poison-row
// contract: on a metadata decode failure, ListMessages returns the
// successfully decoded prefix of the page plus a *sequence.DecodeError
// identifying the row, so the relay can park it (or stop the lane) instead of
// blocking on the whole page.
func TestListMessagesPoisonRowReturnsPrefixAndDecodeError(t *testing.T) {
	if testDB == nil {
		t.Skip("no TiDB")
	}
	truncate(t)

	firstID := publish(t, "ok-1")
	poisonID := publish(t, "poison")
	publish(t, "ok-2")

	st := tidb.NewRelayStore(testDB)
	ctx := context.Background()
	if _, err := st.SequenceMessages(ctx, 100); err != nil {
		t.Fatalf("sequence: %v", err)
	}

	// Corrupt the middle row: '[]' passes the JSON column's validity check but
	// cannot decode into event.Metadata.
	if _, err := testDB.ExecContext(context.Background(), "UPDATE outbox_messages SET metadata = '[]' WHERE seq = 2"); err != nil {
		t.Fatalf("corrupt row: %v", err)
	}

	msgs, err := st.ListMessages(ctx, 0, 10)
	de, ok := errors.AsType[*sequence.DecodeError](err)
	if !ok {
		t.Fatalf("err = %v, want *sequence.DecodeError", err)
	}
	if de.ID != poisonID || de.Seq != 2 {
		t.Fatalf("DecodeError = {ID: %s, Seq: %d}, want {ID: %s, Seq: 2}", de.ID, de.Seq, poisonID)
	}
	if len(msgs) != 1 {
		t.Fatalf("prefix length = %d, want 1 (rows before the poison row)", len(msgs))
	}
	if msgs[0].Metadata.ID != firstID {
		t.Fatalf("prefix[0].Metadata.ID = %s, want %s", msgs[0].Metadata.ID, firstID)
	}
}

// TestListMessagesJSONNullMetadataIsPoison is the regression test for the
// JSON-null bypass: `null` unmarshals into a struct VALUE without error
// (leaving it zero), so before the pointer-target fix a `metadata = 'null'`
// row sailed past the poison classification and went downstream as an empty
// event instead of a *DecodeError.
func TestListMessagesJSONNullMetadataIsPoison(t *testing.T) {
	if testDB == nil {
		t.Skip("no TiDB")
	}
	truncate(t)

	publish(t, "ok-1")
	nullID := publish(t, "null-md")

	st := tidb.NewRelayStore(testDB)
	ctx := context.Background()
	if _, err := st.SequenceMessages(ctx, 100); err != nil {
		t.Fatalf("sequence: %v", err)
	}
	if _, err := testDB.ExecContext(ctx, "UPDATE outbox_messages SET metadata = 'null' WHERE seq = 2"); err != nil {
		t.Fatalf("null row: %v", err)
	}

	msgs, err := st.ListMessages(ctx, 0, 10)
	de, ok := errors.AsType[*sequence.DecodeError](err)
	if !ok {
		t.Fatalf("err = %v, want *sequence.DecodeError (JSON null must classify as poison)", err)
	}
	if de.ID != nullID || de.Seq != 2 {
		t.Fatalf("DecodeError = {ID: %s, Seq: %d}, want {ID: %s, Seq: 2}", de.ID, de.Seq, nullID)
	}
	if len(msgs) != 1 {
		t.Fatalf("prefix length = %d, want 1", len(msgs))
	}
}

// TestListMessagesEmptyObjectMetadataIsPoison is the regression test for the
// JSON-valid-but-empty bypass: '{}' decodes into a NON-nil md with every field
// zero, so it sails past both the decode-error and the JSON-null checks. The
// write side rejects zero Metadata.Time, so such a row is corruption by
// definition and must classify as poison instead of going downstream as an
// empty event.
func TestListMessagesEmptyObjectMetadataIsPoison(t *testing.T) {
	if testDB == nil {
		t.Skip("no TiDB")
	}
	truncate(t)

	publish(t, "ok-1")
	emptyID := publish(t, "empty-md")

	st := tidb.NewRelayStore(testDB)
	ctx := context.Background()
	if _, err := st.SequenceMessages(ctx, 100); err != nil {
		t.Fatalf("sequence: %v", err)
	}
	if _, err := testDB.ExecContext(ctx, "UPDATE outbox_messages SET metadata = '{}' WHERE seq = 2"); err != nil {
		t.Fatalf("empty row: %v", err)
	}

	msgs, err := st.ListMessages(ctx, 0, 10)
	de, ok := errors.AsType[*sequence.DecodeError](err)
	if !ok {
		t.Fatalf("err = %v, want *sequence.DecodeError ('{}' metadata must classify as poison)", err)
	}
	if de.ID != emptyID || de.Seq != 2 {
		t.Fatalf("DecodeError = {ID: %s, Seq: %d}, want {ID: %s, Seq: 2}", de.ID, de.Seq, emptyID)
	}
	if len(msgs) != 1 {
		t.Fatalf("prefix length = %d, want 1", len(msgs))
	}
}

// TestDeleteOffsetUnpinsSweep proves DeleteOffset is the decommissioning step
// for a retired consumer group: its stale offset row pins MIN(last_seq) and
// halts the retention sweep until the row is removed.
func TestDeleteOffsetUnpinsSweep(t *testing.T) {
	if testDB == nil {
		t.Skip("no TiDB")
	}
	truncate(t)

	for range 3 {
		publish(t, "s")
	}
	st := tidb.NewRelayStore(testDB)
	ctx := context.Background()
	if _, err := st.SequenceMessages(ctx, 100); err != nil {
		t.Fatalf("sequence: %v", err)
	}
	if err := st.CommitOffset(ctx, "live", 3); err != nil {
		t.Fatal(err)
	}
	if err := st.CommitOffset(ctx, "retired", 1); err != nil {
		t.Fatal(err)
	}

	// The retired group pins MIN(last_seq) at 1: only seq 1 is sweepable.
	sweepOlderThan := -time.Hour // negative: everything is "older", regardless of insert age
	n, err := st.SweepMessages(ctx, sweepOlderThan, 100)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if n != 1 {
		t.Fatalf("swept %d, want 1 (retired offset row must pin the sweep)", n)
	}

	if err := st.DeleteOffset(ctx, "retired"); err != nil {
		t.Fatalf("delete offset: %v", err)
	}
	off, err := st.Offset(ctx, "retired")
	if err != nil {
		t.Fatal(err)
	}
	if off != 0 {
		t.Fatalf("offset after delete = %d, want 0 (row gone)", off)
	}

	// With the retired row gone, the sweep advances to the live watermark.
	n, err = st.SweepMessages(ctx, sweepOlderThan, 100)
	if err != nil {
		t.Fatalf("sweep after delete: %v", err)
	}
	if n != 2 {
		t.Fatalf("swept %d after DeleteOffset, want 2 (sweep unpinned)", n)
	}

	// Deleting a missing row is a no-op, not an error.
	if err := st.DeleteOffset(ctx, "retired"); err != nil {
		t.Fatalf("delete missing offset: %v", err)
	}
}

// TestGenerateV4RelayedMetadataIDFidelity pins the public outbox.GenerateUUIDv4
// contract (see outbox/sender.go): the row key is a freshly minted UUID, but
// the relayed event still carries exactly the Metadata.ID the caller
// published — the ID travels inside the persisted metadata JSON.
func TestGenerateV4RelayedMetadataIDFidelity(t *testing.T) {
	if testDB == nil {
		t.Skip("no TiDB")
	}
	truncate(t)

	md := newTestMetadata("v4")
	ctx := context.Background()
	tx, err := testDB.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	sender := outbox.NewSender(tidb.NewStore(tx), outbox.WithRowIDGenerator(outbox.GenerateUUIDv4))
	if err := sender.Send(ctx, md, []byte("x")); err != nil {
		_ = tx.Rollback()
		t.Fatalf("publish: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	st := tidb.NewRelayStore(testDB)
	if _, err := st.SequenceMessages(ctx, 100); err != nil {
		t.Fatalf("sequence: %v", err)
	}
	msgs, err := st.ListMessages(ctx, 0, 10)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("got %d messages, want 1", len(msgs))
	}
	if msgs[0].Metadata.ID != md.ID {
		t.Fatalf("Metadata.ID = %s, want the published ID %s (identity must survive GenerateUUIDv4)", msgs[0].Metadata.ID, md.ID)
	}
	if msgs[0].ID == md.ID {
		t.Fatalf("Message.ID = %s equals the published Metadata.ID; GenerateUUIDv4 must mint an independent row key", msgs[0].ID)
	}
}
