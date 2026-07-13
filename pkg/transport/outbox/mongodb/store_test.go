package mongodb_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"

	"github.com/quarks-tech/protoevent-go/pkg/event"
	"github.com/quarks-tech/protoevent-go/pkg/transport/outbox"
	mongodbstore "github.com/quarks-tech/protoevent-go/pkg/transport/outbox/mongodb"
	"github.com/quarks-tech/protoevent-go/pkg/transport/outbox/mongodb/mongodbtest"
)

func TestNewStoreConstructs(t *testing.T) {
	if mongodbstore.NewStore(nil) == nil {
		t.Fatal("NewStore returned nil")
	}
}

func TestNewStoreWithRetention(t *testing.T) {
	if mongodbstore.NewStore(nil, mongodbstore.WithRetention(48*time.Hour)) == nil {
		t.Fatal("NewStore returned nil")
	}
}

// TestCreateOutboxMessageRejectsNilMetadata proves the nil-Metadata guard
// fires before any driver call: a nil *mongo.Database (via NewStore(nil))
// would panic on the first Collection(...) call if the guard didn't run
// first.
func TestCreateOutboxMessageRejectsNilMetadata(t *testing.T) {
	st := mongodbstore.NewStore(nil)
	err := st.CreateOutboxMessage(context.Background(), &outbox.Message{ID: "id"})
	if err == nil {
		t.Fatal("expected error for nil Metadata, got nil")
	}
}

// TestCreateOutboxMessageRejectsEmptyID proves the empty-ID guard fires before
// any driver call (same nil-*mongo.Database trick as the nil-Metadata test).
// Without it, an empty _id inserts fine ONCE and every later publish fails
// with a far-from-cause duplicate-key error.
func TestCreateOutboxMessageRejectsEmptyID(t *testing.T) {
	st := mongodbstore.NewStore(nil)
	err := st.CreateOutboxMessage(context.Background(), &outbox.Message{Metadata: event.NewMetadata("t")})
	if err == nil {
		t.Fatal("expected error for empty ID, got nil")
	}
}

var testDB *mongo.Database

func TestMain(m *testing.M) {
	inst, cleanup, err := mongodbtest.Start(context.Background())
	if err != nil {
		if errors.Is(err, mongodbtest.ErrDockerUnavailable) {
			if os.Getenv("CI") != "" {
				// In CI a missing Docker must fail loudly: os.Exit(0) would
				// silently "pass" the whole module with zero tests run.
				fmt.Fprintf(os.Stderr, "CI is set but Docker is unavailable; refusing to skip mongo integration tests: %v\n", err)
				os.Exit(1)
			}
			fmt.Fprintf(os.Stderr, "skipping mongo integration tests: %v\n", err)
			os.Exit(0)
		}
		fmt.Fprintf(os.Stderr, "mongo integration setup: %v\n", err)
		os.Exit(1) // real harness bug, not a missing Docker
	}
	testDB = inst.DB
	code := m.Run()
	cleanup()
	os.Exit(code)
}

// reset and publish/publishMetadata below take testing.TB (rather than
// *testing.T) so both tests and benchmarks in this package can share them.
func reset(t testing.TB) {
	t.Helper()
	for _, c := range []string{"outbox_messages", "outbox_offsets", "relay_locks"} {
		if err := testDB.Collection(c).Drop(context.Background()); err != nil {
			t.Fatalf("drop %s: %v", c, err)
		}
	}
}

func publish(t testing.TB, subject string) string {
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
// production Sender (mirrors production: same session-txn helper as publish).
// Returns the outbox row/event ID.
func publishMetadata(t testing.TB, md *event.Metadata, data []byte) string {
	t.Helper()
	st := mongodbstore.NewStore(testDB)
	// Publish in a transaction (mirrors production; requires the replica set).
	sess, err := testDB.Client().StartSession()
	if err != nil {
		t.Fatal(err)
	}
	defer sess.EndSession(context.Background())
	_, err = sess.WithTransaction(context.Background(), func(sc context.Context) (any, error) {
		// Go through the production Sender so the default ReuseMetadataID
		// generator (no explicit Message.ID) derives the row id from md.ID.
		return nil, outbox.NewSender(st).Send(sc, md, data)
	})
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	return md.ID
}

func TestPublishInsertsEnvelope(t *testing.T) {
	if testDB == nil {
		t.Skip("no MongoDB")
	}
	reset(t)
	id := publish(t, "s1")
	n, err := testDB.Collection("outbox_messages").CountDocuments(context.Background(), bson.M{"_id": id})
	if err != nil || n != 1 {
		t.Fatalf("count = %d err = %v, want 1", n, err)
	}
}

func TestEnsureIndexesCreatesTTL(t *testing.T) {
	if testDB == nil {
		t.Skip("no MongoDB")
	}
	reset(t)
	if err := mongodbstore.NewStore(testDB).EnsureIndexes(context.Background()); err != nil {
		t.Fatalf("ensure indexes: %v", err)
	}
	cur, err := testDB.Collection("outbox_messages").Indexes().List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var idx []bson.M
	if err := cur.All(context.Background(), &idx); err != nil {
		t.Fatal(err)
	}
	// Same meaning as the unexported mongodb.defaultRetention (7 days, a
	// time.Duration); this is an external test package so it can't reference
	// the constant directly.
	const wantTTLSeconds = int32(7 * 24 * 60 * 60)
	found := false
	for _, ix := range idx {
		// Nested documents decode to bson.D (not bson.M) when the outer
		// document is unmarshaled into a bson.M.
		key, ok := ix["key"].(bson.D)
		if !ok {
			continue
		}
		hasCreateTime := false
		for _, e := range key {
			if e.Key == "create_time" {
				hasCreateTime = true
				break
			}
		}
		if !hasCreateTime {
			continue
		}
		ttl, ok := ix["expireAfterSeconds"]
		if !ok {
			continue
		}
		found = true
		var ttlSeconds int32
		switch v := ttl.(type) {
		case int32:
			ttlSeconds = v
		case int64:
			ttlSeconds = int32(v)
		case float64:
			ttlSeconds = int32(v)
		default:
			t.Fatalf("expireAfterSeconds has unexpected type %T: %v", ttl, ttl)
		}
		if ttlSeconds != wantTTLSeconds {
			t.Fatalf("create_time TTL index expireAfterSeconds = %d, want %d", ttlSeconds, wantTTLSeconds)
		}
	}
	if !found {
		t.Fatal("no TTL index on outbox.create_time")
	}
}

// TestEnsureIndexesRetentionConflictHinted proves the WithRetention knob
// reaches the TTL index, and that changing retention on an EXISTING collection
// surfaces MongoDB's IndexOptionsConflict with a collMod hint instead of
// silently keeping the old TTL.
func TestEnsureIndexesRetentionConflictHinted(t *testing.T) {
	if testDB == nil {
		t.Skip("no MongoDB")
	}
	reset(t)
	ctx := context.Background()

	if err := mongodbstore.NewStore(testDB, mongodbstore.WithRetention(48*time.Hour)).EnsureIndexes(ctx); err != nil {
		t.Fatalf("ensure indexes (48h): %v", err)
	}
	// Re-ensuring with the SAME retention stays idempotent.
	if err := mongodbstore.NewStore(testDB, mongodbstore.WithRetention(48*time.Hour)).EnsureIndexes(ctx); err != nil {
		t.Fatalf("re-ensure with unchanged retention: %v", err)
	}
	// A DIFFERENT retention on the existing index must fail loudly, with the
	// operational hint (collMod) in the message.
	err := mongodbstore.NewStore(testDB, mongodbstore.WithRetention(24*time.Hour)).EnsureIndexes(ctx)
	if err == nil {
		t.Fatal("re-ensure with a different retention succeeded; want IndexOptionsConflict surfaced")
	}
	if !strings.Contains(err.Error(), "collMod") {
		t.Fatalf("conflict error carries no collMod hint: %v", err)
	}
}

func TestOffsetRoundTrip(t *testing.T) {
	if testDB == nil {
		t.Skip("no MongoDB")
	}
	reset(t)
	st := mongodbstore.NewStore(testDB)
	ctx := context.Background()
	ct := time.Now().UTC().Truncate(time.Millisecond)
	if err := st.SaveToken(ctx, "c", "\x01\x02", ct); err != nil {
		t.Fatal(err)
	}
	tok, gotCT, err := st.LoadToken(ctx, "c") // tok is string
	if err != nil {
		t.Fatal(err)
	}
	if len(tok) != 2 || tok[0] != 0x01 || tok[1] != 0x02 {
		t.Fatalf("token = %q, want \\x01\\x02", tok)
	}
	if !gotCT.Equal(ct) {
		t.Fatalf("clusterTime = %v, want %v", gotCT, ct)
	}
}

// TestSaveTokenMonotoneClusterTime proves SaveToken honors the stream.Store
// contract's monotonicity clause: a save carrying an OLDER clusterTime than
// the stored row (a stale leader finishing a slow window) is a silent no-op,
// while a newer one still advances the position.
func TestSaveTokenMonotoneClusterTime(t *testing.T) {
	if testDB == nil {
		t.Skip("no MongoDB")
	}
	reset(t)
	st := mongodbstore.NewStore(testDB)
	ctx := context.Background()

	t1 := time.Now().UTC().Truncate(time.Millisecond)
	t0 := t1.Add(-time.Minute)
	t2 := t1.Add(time.Minute)

	if err := st.SaveToken(ctx, "c", "tok1", t1); err != nil {
		t.Fatalf("save tok1: %v", err)
	}
	// Stale save: older clusterTime must not rewind the stored row — and must
	// not error either (the no-op is deliberate, not a failure).
	if err := st.SaveToken(ctx, "c", "tok0", t0); err != nil {
		t.Fatalf("stale save returned an error, want silent no-op: %v", err)
	}
	tok, gotCT, err := st.LoadToken(ctx, "c")
	if err != nil {
		t.Fatal(err)
	}
	if tok != "tok1" || !gotCT.Equal(t1) {
		t.Fatalf("after stale save: token = %q ct = %v, want tok1 %v (rewound!)", tok, gotCT, t1)
	}
	// Newer save still advances.
	if err := st.SaveToken(ctx, "c", "tok2", t2); err != nil {
		t.Fatalf("save tok2: %v", err)
	}
	tok, gotCT, err = st.LoadToken(ctx, "c")
	if err != nil {
		t.Fatal(err)
	}
	if tok != "tok2" || !gotCT.Equal(t2) {
		t.Fatalf("after newer save: token = %q ct = %v, want tok2 %v", tok, gotCT, t2)
	}
	// EQUAL clusterTime, different token: must UPDATE ($lte's equality path).
	// Event clusterTimes carry whole-second granularity, so a train of events
	// committed in the same second all save at the same clusterTime — treating
	// equality as stale would freeze the token at the first of them.
	if err := st.SaveToken(ctx, "c", "tok3", t2); err != nil {
		t.Fatalf("equal-clusterTime save: %v", err)
	}
	tok, gotCT, err = st.LoadToken(ctx, "c")
	if err != nil {
		t.Fatal(err)
	}
	if tok != "tok3" || !gotCT.Equal(t2) {
		t.Fatalf("after equal-clusterTime save: token = %q ct = %v, want tok3 %v", tok, gotCT, t2)
	}
}

// TestSaveTokenRejectsEmptyToken proves the store defends its own invariant:
// an empty token is never a valid position (LoadToken maps "" to "no stored
// position"), so saving one would erase the group's persisted offset and make
// the next Watch restart "at now", silently skipping events. The save must
// error and leave the stored row untouched.
func TestSaveTokenRejectsEmptyToken(t *testing.T) {
	if testDB == nil {
		t.Skip("no MongoDB")
	}
	reset(t)
	st := mongodbstore.NewStore(testDB)
	ctx := context.Background()

	ct := time.Now().UTC().Truncate(time.Millisecond)
	if err := st.SaveToken(ctx, "c", "tok1", ct); err != nil {
		t.Fatalf("save tok1: %v", err)
	}
	if err := st.SaveToken(ctx, "c", "", ct.Add(time.Minute)); err == nil {
		t.Fatal("SaveToken with empty token succeeded; want an error")
	}
	tok, gotCT, err := st.LoadToken(ctx, "c")
	if err != nil {
		t.Fatal(err)
	}
	if tok != "tok1" || !gotCT.Equal(ct) {
		t.Fatalf("stored row changed after rejected empty-token save: token = %q ct = %v, want tok1 %v", tok, gotCT, ct)
	}
}

// TestSaveTokenHealsRowWithoutClusterTime proves a stored row LACKING
// cluster_time (never written by this package — external tampering or manual
// repair) does not stall the group forever: without the $exists guard the
// monotone $lte filter never matches such a row, the upsert hits the _id
// index, and the duplicate-key error is swallowed as "stale" — freezing the
// token silently. The next save must instead update the row in place.
func TestSaveTokenHealsRowWithoutClusterTime(t *testing.T) {
	if testDB == nil {
		t.Skip("no MongoDB")
	}
	reset(t)
	ctx := context.Background()

	// Hand-insert a legacy/damaged row via the raw driver: token bytes present,
	// cluster_time absent.
	if _, err := testDB.Collection("outbox_offsets").InsertOne(ctx, bson.M{
		"_id":          "c",
		"resume_token": []byte("legacy"),
	}); err != nil {
		t.Fatalf("hand-insert legacy row: %v", err)
	}

	st := mongodbstore.NewStore(testDB)
	ct := time.Now().UTC().Truncate(time.Millisecond)
	if err := st.SaveToken(ctx, "c", "tok1", ct); err != nil {
		t.Fatalf("save over legacy row: %v", err)
	}
	tok, gotCT, err := st.LoadToken(ctx, "c")
	if err != nil {
		t.Fatal(err)
	}
	if tok != "tok1" || !gotCT.Equal(ct) {
		t.Fatalf("legacy row not healed: token = %q ct = %v, want tok1 %v", tok, gotCT, ct)
	}
}

func TestLeaderLockMutualExclusionAndRelease(t *testing.T) {
	if testDB == nil {
		t.Skip("no MongoDB")
	}
	reset(t)
	st := mongodbstore.NewStore(testDB)
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

// TestLeaderLockRenewalSameHolder proves the renewal path: the SAME holder
// re-acquiring an already-live (not expired) lock document must succeed via
// the update branch of TryAcquireLeaderLock, not just the initial-upsert
// branch that TestLeaderLockMutualExclusionAndRelease exercises. Every relay
// tick's TryAcquire call hits exactly this path once leadership is held.
func TestLeaderLockRenewalSameHolder(t *testing.T) {
	if testDB == nil {
		t.Skip("no MongoDB")
	}
	reset(t)
	st := mongodbstore.NewStore(testDB)
	ctx := context.Background()

	okA1, err := st.TryAcquireLeaderLock(ctx, "lock", "A", 30*time.Second)
	if err != nil || !okA1 {
		t.Fatalf("A first acquire = %v, %v; want true", okA1, err)
	}
	// A renews on the still-live document (the update path every relay tick
	// exercises), not a fresh upsert.
	okA2, err := st.TryAcquireLeaderLock(ctx, "lock", "A", 30*time.Second)
	if err != nil || !okA2 {
		t.Fatalf("A renewal = %v, %v; want true", okA2, err)
	}

	// B must still be denied: A's renewal must not have released or handed
	// off the lock.
	okB, err := st.TryAcquireLeaderLock(ctx, "lock", "B", 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if okB {
		t.Fatal("B acquired while A holds a renewed lock")
	}
}

// TestLeaderLockExpiredLeaseTakeover proves a standby takes over once the
// lease expires — with the expiry decided by the SERVER clock ($$NOW), the
// same clock that stamped the deadline, so the takeover works regardless of
// skew between relay instances' local clocks.
func TestLeaderLockExpiredLeaseTakeover(t *testing.T) {
	if testDB == nil {
		t.Skip("no MongoDB")
	}
	reset(t)
	st := mongodbstore.NewStore(testDB)
	ctx := context.Background()

	okA, err := st.TryAcquireLeaderLock(ctx, "lock", "A", 200*time.Millisecond)
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
	// Wait out A's lease (comfortably past 200ms), then B must win.
	time.Sleep(700 * time.Millisecond)
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
