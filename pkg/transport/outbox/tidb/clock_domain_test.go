package tidb_test

import (
	"context"
	"database/sql"
	"errors"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/quarks-tech/protoevent-go/pkg/transport/outbox/tidb"
)

// poolInZone opens an isolated pool whose session time_zone is zone.
//
// A separate *sql.DB, not a connection borrowed from testDB: sql.Conn.Close()
// returns a connection to its pool with session state intact, so a session variable
// set on a borrowed conn leaks into every later test that draws it.
// SetMaxOpenConns(1) makes the SET below govern every statement the pool runs.
func poolInZone(t *testing.T, zone string) *sql.DB {
	t.Helper()

	db, err := sql.Open("mysql", testDSN)
	if err != nil {
		t.Fatalf("open pool in %s: %v", zone, err)
	}
	t.Cleanup(func() { _ = db.Close() })
	db.SetMaxOpenConns(1)

	if _, err := db.ExecContext(context.Background(), "SET SESSION time_zone = ?", zone); err != nil {
		t.Fatalf("set time_zone %s: %v", zone, err)
	}

	var got string
	if err := db.QueryRowContext(context.Background(), "SELECT @@session.time_zone").Scan(&got); err != nil {
		t.Fatalf("read back time_zone: %v", err)
	}
	if got != zone {
		t.Fatalf("session time_zone = %q, want %q", got, zone)
	}

	return db
}

// lockHolder reports who currently holds the lock row.
func lockHolder(t *testing.T, name string) string {
	t.Helper()

	var holder string
	if err := testDB.QueryRowContext(context.Background(),
		"SELECT holder_id FROM relay_locks WHERE name = ?", name).Scan(&holder); err != nil {
		t.Fatalf("read lock holder: %v", err)
	}

	return holder
}

// TestLeaderLockIsNotFooledByASessionTimeZone is the S13 regression test: the leader
// lease must not depend on the SESSION TIME ZONE of whoever evaluates it.
//
// expire_time is DATETIME(6), which carries no zone, and the lease was written and
// compared with NOW(6) — a value rendered in the *session's* time_zone. Two sessions
// in different zones therefore compare incommensurable numbers: a session 8 hours
// ahead sees every live lease as long expired and steals it, while a session behind
// sees an expired lease as live and never takes over.
//
// Session time_zone is a faithful, single-container proxy for the production hazard.
// Each tidb-server renders NOW(6) from its own OS clock in its own
// system_time_zone, so two app pods behind a load balancer, or one DSN carrying an
// explicit time_zone, produce exactly this divergence — and the outcome is two live
// leaders whose unfenced offset commits interleave, which breaks total order for good.
//
// TestLeaderLockExpiredLeaseTakeover asserts the OPPOSITE in its own comment
// ("expiry is decided by the SERVER clock (NOW(6)), the same clock that stamped the
// deadline, so the takeover works regardless of skew"). That holds only because the
// harness boots one tidb-server and every test shares one DSN.
func TestLeaderLockIsNotFooledByASessionTimeZone(t *testing.T) {
	if testDB == nil {
		t.Skip("no TiDB")
	}

	t.Run("a session ahead cannot steal a live lease", func(t *testing.T) {
		truncate(t)
		ctx := context.Background()

		utcDB := poolInZone(t, "+00:00")
		aheadDB := poolInZone(t, "+08:00")

		ok, err := tidb.NewRelayStore(utcDB).TryAcquireLeaderLock(ctx, "tz", "A", 30*time.Second)
		if err != nil {
			t.Fatalf("A acquire: %v", err)
		}
		if !ok {
			t.Fatal("A failed to acquire a free lock")
		}

		// B's lease request arrives through a session whose clock reads 8 hours
		// later. A's lease has 30 seconds left and must be respected.
		stolen, err := tidb.NewRelayStore(aheadDB).TryAcquireLeaderLock(ctx, "tz", "B", 30*time.Second)
		if err != nil {
			t.Fatalf("B acquire: %v", err)
		}
		if stolen {
			t.Error("B stole a live lease through a session 8 hours ahead: two relays now believe " +
				"they lead, and their unfenced offset commits interleave")
		}
		if h := lockHolder(t, "tz"); h != "A" {
			t.Errorf("lock holder = %q, want \"A\"", h)
		}
	})

	t.Run("a session behind can still take over an expired lease", func(t *testing.T) {
		truncate(t)
		ctx := context.Background()

		utcDB := poolInZone(t, "+00:00")
		aheadDB := poolInZone(t, "+08:00")

		// The incumbent's deadline is stamped by a session 8 hours ahead.
		const shortTTL = 300 * time.Millisecond
		ok, err := tidb.NewRelayStore(aheadDB).TryAcquireLeaderLock(ctx, "tz2", "Ahead", shortTTL)
		if err != nil {
			t.Fatalf("Ahead acquire: %v", err)
		}
		if !ok {
			t.Fatal("Ahead failed to acquire a free lock")
		}

		// Wait the lease out with a generous margin.
		time.Sleep(4 * shortTTL)

		// A standby in UTC must now win. If the deadline is read in the wrong zone it
		// looks 8 hours away, and this relay group stalls until somebody notices.
		took, err := tidb.NewRelayStore(utcDB).TryAcquireLeaderLock(ctx, "tz2", "Utc", 30*time.Second)
		if err != nil {
			t.Fatalf("Utc acquire: %v", err)
		}
		if !took {
			t.Error("a standby could not take over a lease that expired 1.2s ago, because the " +
				"deadline was stamped in another session's time zone: the group stalls with a " +
				"lease that looks live for hours")
		}
	})
}

// TestRetentionSweepIsNotFooledByASessionTimeZone is the second half of S13: the
// retention cutoff must not depend on the sweeper's session time zone either.
//
// The sweep deletes rows whose create_time is older than the retention window, with
// both sides evaluated as NOW(6). A sweeper 8 hours ahead of the session that stamped
// create_time computes a cutoff 8 hours late, so it deletes rows far younger than the
// configured window — shrinking the break-glass replay window that the ErrHistoryLost
// runbook depends on.
//
// The window here is one hour and the rows are seconds old, so a correct sweep must
// delete nothing at all.
func TestRetentionSweepIsNotFooledByASessionTimeZone(t *testing.T) {
	if testDB == nil {
		t.Skip("no TiDB")
	}
	truncate(t)
	ctx := context.Background()

	// Publish and sequence through the default (UTC) pool.
	const rows = 3
	for range rows {
		publish(t, "tz-sweep")
	}
	st := tidb.NewRelayStore(testDB)
	if _, err := st.SequenceMessages(ctx, 100); err != nil {
		t.Fatalf("sequence: %v", err)
	}
	msgs, err := st.ListMessages(ctx, 0, 100)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	// Deliver everything, so the sweep is not held back by the offset floor and the
	// only thing deciding deletion is the create_time cutoff.
	if err := st.CommitOffset(ctx, "g", msgs[len(msgs)-1].Seq); err != nil {
		t.Fatalf("commit offset: %v", err)
	}

	aheadDB := poolInZone(t, "+08:00")
	sweeper := tidb.NewRelayStore(aheadDB, tidb.WithRetentionWindow(time.Hour))

	swept, err := sweeper.SweepMessages(ctx, 100)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if swept != 0 {
		t.Errorf("a sweeper 8 hours ahead deleted %d of %d rows that are seconds old under a "+
			"1h retention window: retention is being decided in the sweeper's time zone", swept, rows)
	}

	var left int
	if err := testDB.QueryRowContext(ctx, "SELECT COUNT(*) FROM outbox_messages").Scan(&left); err != nil {
		t.Fatalf("count remaining: %v", err)
	}
	if left != rows {
		t.Errorf("%d of %d rows survived the sweep, want all of them", left, rows)
	}
}

// TestLeaseExpiryIsDeterministicWithAnInjectedClock demonstrates what moving the
// lease off the database clock bought: expiry is now testable by advancing a
// variable instead of sleeping.
//
// Every existing lease test waits out a real TTL (TestLeaderLockExpiredLeaseTakeover
// sleeps 4x300ms), which is both slow and inherently racy — a loaded CI box can make
// a "live" lease expire mid-test. Here the entire handover is driven at a fixed
// instant, so it cannot flake and needs no margin.
func TestLeaseExpiryIsDeterministicWithAnInjectedClock(t *testing.T) {
	if testDB == nil {
		t.Skip("no TiDB")
	}
	truncate(t)
	ctx := context.Background()

	// A clock the test owns outright.
	base := time.Date(2026, 8, 18, 12, 0, 0, 0, time.UTC)
	var offset time.Duration
	clock := func() time.Time { return base.Add(offset) }

	advance := func(d time.Duration) { offset += d }

	st := tidb.NewRelayStore(testDB, tidb.WithClock(clock))

	const ttl = 30 * time.Second

	ok, err := st.TryAcquireLeaderLock(ctx, "det", "A", ttl)
	if err != nil {
		t.Fatalf("A acquire: %v", err)
	}
	if !ok {
		t.Fatal("A failed to acquire a free lock")
	}

	// Still inside the lease: no takeover, no waiting.
	advance(ttl - time.Second)
	stolen, err := st.TryAcquireLeaderLock(ctx, "det", "B", ttl)
	if err != nil {
		t.Fatalf("B acquire inside the lease: %v", err)
	}
	if stolen {
		t.Fatal("B took the lock one second before A's lease expired")
	}

	// A renews, which must move the deadline forward from the CURRENT time.
	if ok, err := st.TryAcquireLeaderLock(ctx, "det", "A", ttl); err != nil || !ok {
		t.Fatalf("A renew = %v, %v; want true", ok, err)
	}

	// Past the original deadline but inside the renewed one.
	advance(2 * time.Second)
	stolen, err = st.TryAcquireLeaderLock(ctx, "det", "B", ttl)
	if err != nil {
		t.Fatalf("B acquire after renewal: %v", err)
	}
	if stolen {
		t.Fatal("B took the lock after A renewed: the renewal did not extend the deadline")
	}

	// Now step past the renewed deadline. B must win, and A must be denied.
	advance(ttl)
	took, err := st.TryAcquireLeaderLock(ctx, "det", "B", ttl)
	if err != nil {
		t.Fatalf("B acquire after expiry: %v", err)
	}
	if !took {
		t.Fatal("B failed to take over an expired lease")
	}
	if h := lockHolder(t, "det"); h != "B" {
		t.Fatalf("lock holder = %q, want \"B\"", h)
	}

	if regained, err := st.TryAcquireLeaderLock(ctx, "det", "A", ttl); err != nil || regained {
		t.Fatalf("A re-acquired while B holds a live lease (ok=%v, err=%v)", regained, err)
	}
}

// TestFencedCommitRejectsASupersededHolder proves the fenced upsert at the SQL level.
//
// No fake can cover this: the guarantee lives in an INSERT ... SELECT over the lock
// row whose ON DUPLICATE KEY UPDATE must not fire when the SELECT is empty, and in
// MySQL's affected-rows semantics, which the store has to read correctly to tell
// "fenced out" from "matched but changed nothing".
func TestFencedCommitRejectsASupersededHolder(t *testing.T) {
	if testDB == nil {
		t.Skip("no TiDB")
	}
	truncate(t)
	ctx := context.Background()

	st := tidb.NewRelayStore(testDB)

	const (
		group = "g"
		lock  = "g-lock"
	)

	if ok, err := st.TryAcquireLeaderLock(ctx, lock, "A", time.Minute); err != nil || !ok {
		t.Fatalf("A acquire = %v, %v; want true", ok, err)
	}

	// The holder commits: applied, and reported as applied.
	persisted, err := st.CommitOffsetFenced(ctx, group, lock, "A", 10)
	if err != nil {
		t.Fatalf("A commit: %v", err)
	}
	if !persisted {
		t.Fatal("the lock holder's commit was reported as not persisted")
	}
	if got := offsetOf(t); got != 10 {
		t.Fatalf("offset = %d, want 10", got)
	}

	// A stranger is fenced out, and told so.
	persisted, err = st.CommitOffsetFenced(ctx, group, lock, "B", 99)
	if err != nil {
		t.Fatalf("B commit: %v", err)
	}
	if persisted {
		t.Error("a non-holder's commit was reported as persisted")
	}
	if got := offsetOf(t); got != 10 {
		t.Errorf("offset = %d, want 10: a non-holder moved the watermark", got)
	}

	// Now B legitimately takes over, and A is superseded: A's commit must be rejected
	// even though A still believes it leads.
	if ok, err := st.TryAcquireLeaderLock(ctx, lock, "B", time.Minute); err != nil || ok {
		t.Fatalf("B acquire while A's lease is live = %v, %v; want false", ok, err)
	}
	if _, err := testDB.ExecContext(ctx,
		"UPDATE relay_locks SET holder_id = 'B' WHERE name = ?", lock); err != nil {
		t.Fatalf("hand over the lock: %v", err)
	}

	persisted, err = st.CommitOffsetFenced(ctx, group, lock, "A", 50)
	if err != nil {
		t.Fatalf("superseded A commit: %v", err)
	}
	if persisted {
		t.Error("a superseded holder's commit was reported as persisted")
	}
	if got := offsetOf(t); got != 10 {
		t.Errorf("offset = %d, want 10: a superseded holder moved the watermark", got)
	}

	// A repeat commit at the SAME seq by the real holder is a no-op UPDATE, which
	// reports zero affected rows. That must NOT read as "fenced out" — the store
	// disambiguates by re-reading the holder. This is the case a naive
	// RowsAffected() > 0 check gets wrong.
	if _, err := st.CommitOffsetFenced(ctx, group, lock, "B", 60); err != nil {
		t.Fatalf("B first commit: %v", err)
	}
	persisted, err = st.CommitOffsetFenced(ctx, group, lock, "B", 60)
	if err != nil {
		t.Fatalf("B repeat commit: %v", err)
	}
	if !persisted {
		t.Error("a repeated commit at the same seq by the true holder was reported as not " +
			"persisted: zero affected rows was misread as a lost election")
	}

	// Monotonicity still holds under the fence.
	if _, err := st.CommitOffsetFenced(ctx, group, lock, "B", 5); err != nil {
		t.Fatalf("B lower commit: %v", err)
	}
	if got := offsetOf(t); got != 60 {
		t.Errorf("offset = %d, want 60: the fenced upsert is not monotone", got)
	}

	// A missing lock row fences everyone out: no row means no holder to match.
	if _, err := testDB.ExecContext(ctx, "DELETE FROM relay_locks WHERE name = ?", lock); err != nil {
		t.Fatalf("delete lock: %v", err)
	}
	persisted, err = st.CommitOffsetFenced(ctx, group, lock, "B", 70)
	if err != nil {
		t.Fatalf("commit with no lock row: %v", err)
	}
	if persisted {
		t.Error("a commit was reported as persisted with no lock row at all")
	}
	if got := offsetOf(t); got != 60 {
		t.Errorf("offset = %d, want 60", got)
	}
}

// offsetOrZero reads a consumer group's watermark, treating a group that has not been
// registered yet as 0. Polling for progress has to tolerate the window before the relay
// primes its offset row.
func offsetOrZero(t *testing.T, group string) int64 {
	t.Helper()

	var seq int64
	err := testDB.QueryRowContext(context.Background(),
		"SELECT last_seq FROM outbox_offsets WHERE name = ?", group).Scan(&seq)
	if errors.Is(err, sql.ErrNoRows) {
		return 0
	}
	if err != nil {
		t.Fatalf("read offset of %s: %v", group, err)
	}

	return seq
}

// offsetOf reads the committed watermark of the group used by the tests in this file.
// Use offsetOrZero when the group may not be registered yet.
func offsetOf(t *testing.T) int64 {
	t.Helper()

	const group = "g"

	var seq int64
	if err := testDB.QueryRowContext(context.Background(),
		"SELECT last_seq FROM outbox_offsets WHERE name = ?", group).Scan(&seq); err != nil {
		t.Fatalf("read offset of %s: %v", group, err)
	}

	return seq
}

// TestConcurrentSequencersHoldUnderOptimisticTxnMode is the S12 regression test.
//
// SequenceMessages is a client-side read-modify-write across three round trips, and its
// safety claim is that "the counter row is locked FOR UPDATE for the whole pass, so
// concurrent sequencers serialize and can never double-assign". On TiDB that holds only
// in PESSIMISTIC mode. Under tidb_txn_mode='optimistic' — set globally on many clusters
// migrated from MySQL, or per-session by a shared DSN or a proxy — SELECT ... FOR UPDATE
// takes no lock at read time and the conflict surfaces at COMMIT as error 9007.
//
// The failure is not a one-off retryable blip: every relay runs a sequencer by default,
// so contended passes return an error every tick while delivering nothing. And with
// transaction auto-retry enabled, TiDB replays the statements with the SAME bound
// parameters — next_seq was computed in Go from the first read and is not recomputed —
// so the replay re-assigns an already-used range and the unique index rejects it as
// 1062.
//
// TestConcurrentSequencersNoDuplicateNoGap runs four goroutines against the container's
// DEFAULT (pessimistic) mode and treats any error as fatal, so it asserts the happy path
// only. The mode was never varied.
func TestConcurrentSequencersHoldUnderOptimisticTxnMode(t *testing.T) {
	if testDB == nil {
		t.Skip("no TiDB")
	}
	truncate(t)
	ctx := context.Background()

	const rows = 200
	for i := range rows {
		publish(t, "opt-"+strconv.Itoa(i))
	}

	// A pool pinned to optimistic mode, isolated so the session variable cannot leak
	// into the pool every other test shares.
	optDB, err := sql.Open("mysql", testDSN)
	if err != nil {
		t.Fatalf("open optimistic pool: %v", err)
	}
	defer func() { _ = optDB.Close() }()
	// Several connections: the point is concurrent sequencers contending.
	optDB.SetMaxOpenConns(4)
	// Capture and restore the cluster's ACTUAL default rather than assuming one: this
	// is a global, so getting the restore wrong would silently reconfigure every later
	// test in the binary.
	var original string
	if err := optDB.QueryRowContext(ctx, "SELECT @@global.tidb_txn_mode").Scan(&original); err != nil {
		t.Fatalf("read global txn mode: %v", err)
	}
	if _, err := optDB.ExecContext(ctx, "SET GLOBAL tidb_txn_mode = 'optimistic'"); err != nil {
		t.Fatalf("set global txn mode: %v", err)
	}
	t.Cleanup(func() {
		if _, err := testDB.ExecContext(context.Background(),
			"SET GLOBAL tidb_txn_mode = ?", original); err != nil {
			t.Logf("restore global txn mode to %q: %v", original, err)
		}
	})
	// A fresh pool so every connection picks up the new global.
	runDB, err := sql.Open("mysql", testDSN)
	if err != nil {
		t.Fatalf("open run pool: %v", err)
	}
	defer func() { _ = runDB.Close() }()
	runDB.SetMaxOpenConns(4)

	var mode string
	if err := runDB.QueryRowContext(ctx, "SELECT @@tidb_txn_mode").Scan(&mode); err != nil {
		t.Fatalf("read txn mode: %v", err)
	}
	if mode != "optimistic" {
		t.Fatalf("tidb_txn_mode = %q, want optimistic: this test is not exercising the mode it names", mode)
	}

	st := tidb.NewRelayStore(runDB)

	var (
		mu    sync.Mutex
		errs  []error
		total int
		wg    sync.WaitGroup
	)
	for range 4 {
		wg.Go(func() {
			for {
				n, err := st.SequenceMessages(ctx, 20)
				if err != nil {
					mu.Lock()
					errs = append(errs, err)
					mu.Unlock()

					return
				}
				if n == 0 {
					return
				}
				mu.Lock()
				total += n
				mu.Unlock()
			}
		})
	}
	wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	if len(errs) > 0 {
		t.Errorf("%d of 4 concurrent sequencers failed under optimistic mode; first: %v", len(errs), errs[0])
	}
	if total != rows {
		t.Errorf("sequenced %d rows in total, want %d", total, rows)
	}

	// The invariant that actually matters, whatever the mode did: dense, no gap, no
	// duplicate. A gap means a consumer's seq > ? cursor steps over an event forever.
	assertDenseSeq(t, rows)
}

// TestFailedSequencingPassLeavesNoOpenTransaction guards the hazard that comes with
// driving the sequencing transaction by hand.
//
// BEGIN PESSIMISTIC has to be issued as a statement, so the transaction is invisible to
// database/sql: it will not roll anything back when the connection is released. A pass
// that returns an error without cleaning up would hand the next borrower a connection
// with an open transaction and its locks still held — and on a pool of one, the next
// borrower is the very next call.
//
// The pool is pinned to a single connection precisely so a leak cannot hide behind a
// second one.
func TestFailedSequencingPassLeavesNoOpenTransaction(t *testing.T) {
	if testDB == nil {
		t.Skip("no TiDB")
	}
	truncate(t)
	ctx := context.Background()

	oneConn, err := sql.Open("mysql", testDSN)
	if err != nil {
		t.Fatalf("open single-conn pool: %v", err)
	}
	defer func() { _ = oneConn.Close() }()
	oneConn.SetMaxOpenConns(1)

	st := tidb.NewRelayStore(oneConn)

	publish(t, "leak-1")

	// Remove the counter row so the pass fails AFTER BEGIN, inside the transaction.
	if _, err := testDB.ExecContext(ctx, "DELETE FROM outbox_sequencers"); err != nil {
		t.Fatalf("delete sequencer row: %v", err)
	}

	if _, err := st.SequenceMessages(ctx, 100); err == nil {
		t.Fatal("sequencing succeeded with no counter row, so this test is not exercising a failure")
	}

	// Restore the row through the OTHER pool. If the failed pass left its transaction
	// open, it still holds locks and this write blocks until the lock-wait timeout.
	restore, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if _, err := testDB.ExecContext(restore,
		"INSERT INTO outbox_sequencers (name, next_seq) VALUES ('default', 1)"); err != nil {
		t.Fatalf("restore counter row (a blocked write here means the failed pass held its locks): %v", err)
	}

	// And the same single connection must be immediately reusable.
	reuse, cancel2 := context.WithTimeout(ctx, 10*time.Second)
	defer cancel2()
	n, err := st.SequenceMessages(reuse, 100)
	if err != nil {
		t.Fatalf("the connection was not reusable after a failed pass: %v", err)
	}
	if n != 1 {
		t.Fatalf("sequenced %d rows, want 1", n)
	}
	assertDenseSeq(t, 1)
}
