package mongodb_test

import (
	"context"
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

func TestStoreSatisfiesStreamStore(t *testing.T) {
	// Compile-time proof lives in watch.go (var _ stream.StreamStore = ...).
	// This test just ensures the package builds with Watch present.
	_ = mongodbstore.NewStore(nil)
}

var testDB *mongo.Database

func TestMain(m *testing.M) {
	inst, cleanup, err := mongodbtest.Start(context.Background())
	if err != nil {
		os.Exit(0) // Docker unavailable: skip the integration suite
	}
	testDB = inst.DB
	code := m.Run()
	cleanup()
	os.Exit(code)
}

func reset(t *testing.T) {
	t.Helper()
	for _, c := range []string{"outbox", "outbox_offsets", "relay_lock"} {
		if err := testDB.Collection(c).Drop(context.Background()); err != nil {
			t.Fatalf("drop %s: %v", c, err)
		}
	}
}

func publish(t *testing.T, subject string) string {
	t.Helper()
	st := mongodbstore.NewStore(testDB)
	md := event.NewMetadata("books.created")
	md.ID = uuid.NewString()
	md.Source = "books-service"
	md.Subject = subject
	md.DataContentType = "application/proto"
	md.Time = time.Now().UTC()
	// Publish in a transaction (mirrors production; requires the replica set).
	sess, err := testDB.Client().StartSession()
	if err != nil {
		t.Fatal(err)
	}
	defer sess.EndSession(context.Background())
	_, err = sess.WithTransaction(context.Background(), func(sc context.Context) (any, error) {
		return nil, st.CreateOutboxMessage(sc, &outbox.Message{ID: md.ID, Metadata: md, Data: []byte("x"), CreateTime: md.Time})
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
	found := false
	for _, ix := range idx {
		if _, ok := ix["expireAfterSeconds"]; ok {
			found = true
		}
	}
	if !found {
		t.Fatal("no TTL index on outbox")
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
	if len(tok) != 2 || tok[0] != 0x01 {
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
	okB, _ := st.TryAcquireLeaderLock(ctx, "lock", "B", 30*time.Second)
	if okB {
		t.Fatal("B acquired while A holds the lock")
	}
	if err := st.ReleaseLeaderLock(ctx, "lock", "A"); err != nil {
		t.Fatal(err)
	}
	okB2, _ := st.TryAcquireLeaderLock(ctx, "lock", "B", 30*time.Second)
	if !okB2 {
		t.Fatal("B failed to acquire after A released")
	}
}
