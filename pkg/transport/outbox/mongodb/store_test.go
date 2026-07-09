package mongodb_test

import (
	"context"
	"errors"
	"fmt"
	"os"
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

func TestNewStoreWithMaxAwaitTime(t *testing.T) {
	if mongodbstore.NewStore(nil, mongodbstore.WithMaxAwaitTime(300*time.Millisecond)) == nil {
		t.Fatal("NewStore returned nil")
	}
}

func TestStoreSatisfiesStreamStore(t *testing.T) {
	// Compile-time proof lives in watch.go (var _ stream.StreamStore = ...).
	// This test just ensures the package builds with Watch present.
	_ = mongodbstore.NewStore(nil)
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

var testDB *mongo.Database

func TestMain(m *testing.M) {
	inst, cleanup, err := mongodbtest.Start(context.Background())
	if err != nil {
		if errors.Is(err, mongodbtest.ErrDockerUnavailable) {
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
	for _, c := range []string{"outbox", "outbox_offsets", "relay_lock"} {
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
	n, err := testDB.Collection("outbox").CountDocuments(context.Background(), bson.M{"_id": id})
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
	cur, err := testDB.Collection("outbox").Indexes().List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var idx []bson.M
	if err := cur.All(context.Background(), &idx); err != nil {
		t.Fatal(err)
	}
	// Same meaning as the unexported mongodb.retentionSeconds (7 days); this is
	// an external test package so it can't reference the constant directly.
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
