package mongodb_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/quarks-tech/protoevent-go/pkg/event"
	"github.com/quarks-tech/protoevent-go/pkg/transport/outbox"
	mongodbstore "github.com/quarks-tech/protoevent-go/pkg/transport/outbox/mongodb"
)

// TestWithCollectionPrefixPanicsOnInvalid pins the identifier guard, kept
// deliberately as strict as the TiDB table-prefix rule so one prefix value
// works verbatim across both backends.
func TestWithCollectionPrefixPanicsOnInvalid(t *testing.T) {
	for _, bad := range []string{"", "1orders_", "orders-", "orders ", "a$b", strings.Repeat("p", 41)} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("WithCollectionPrefix(%q) did not panic", bad)
				}
			}()
			mongodbstore.WithCollectionPrefix(bad)
		}()
	}
	mongodbstore.WithCollectionPrefix("orders_")
}

// TestPrefixedOutboxCoexistsInOneDatabase proves the multi-instance design on
// MongoDB: a WithCollectionPrefix instance indexes, publishes transactionally,
// and tracks its resume token in its own collections — invisible to the
// default instance in the same database.
func TestPrefixedOutboxCoexistsInOneDatabase(t *testing.T) {
	if testDB == nil {
		t.Skip("no MongoDB")
	}
	reset(t)
	ctx := context.Background()
	for _, c := range []string{"orders_outbox_messages", "orders_outbox_offsets", "orders_relay_locks"} {
		if err := testDB.Collection(c).Drop(ctx); err != nil {
			t.Fatalf("drop %s: %v", c, err)
		}
	}

	st := mongodbstore.NewStore(testDB, mongodbstore.WithCollectionPrefix("orders_"))
	if err := st.EnsureIndexes(ctx); err != nil {
		t.Fatalf("EnsureIndexes on prefixed instance: %v", err)
	}

	// Publish through the prefixed store inside a session transaction.
	sess, err := testDB.Client().StartSession()
	if err != nil {
		t.Fatal(err)
	}
	defer sess.EndSession(ctx)
	md := newPrefixTestMetadata()
	if _, err := sess.WithTransaction(ctx, func(sc context.Context) (any, error) {
		return nil, outbox.NewSender(st).Send(sc, md, []byte("x"))
	}); err != nil {
		t.Fatalf("prefixed publish: %v", err)
	}

	// The envelope landed in the prefixed collection — and ONLY there.
	prefixedCount, err := testDB.Collection("orders_outbox_messages").CountDocuments(ctx, bson.M{})
	if err != nil {
		t.Fatal(err)
	}
	if prefixedCount != 1 {
		t.Fatalf("orders_outbox_messages count = %d, want 1", prefixedCount)
	}
	defaultCount, err := testDB.Collection("outbox_messages").CountDocuments(ctx, bson.M{})
	if err != nil {
		t.Fatal(err)
	}
	if defaultCount != 0 {
		t.Fatalf("outbox_messages count = %d, want 0 (instances must not cross)", defaultCount)
	}

	// Resume tokens are instance-local too: a save through the prefixed store
	// is invisible to a default-instance LoadToken for the same group name.
	ct := time.Now().UTC().Truncate(time.Millisecond)
	if persisted, err := st.SaveToken(ctx, "orders-relay", "tok1", ct); err != nil || !persisted {
		t.Fatalf("prefixed SaveToken: persisted=%v err=%v", persisted, err)
	}
	tok, _, err := st.LoadToken(ctx, "orders-relay")
	if err != nil || tok != "tok1" {
		t.Fatalf("prefixed LoadToken = %q, %v; want tok1", tok, err)
	}
	defaultTok, _, err := mongodbstore.NewStore(testDB).LoadToken(ctx, "orders-relay")
	if err != nil {
		t.Fatal(err)
	}
	if defaultTok != "" {
		t.Fatalf("default-instance LoadToken = %q, want \"\" (offsets must not cross)", defaultTok)
	}
}

// newPrefixTestMetadata builds a publishable metadata envelope without
// depending on the shared helpers' store wiring.
func newPrefixTestMetadata() *event.Metadata {
	md := event.NewMetadata("orders.created")
	md.ID = uuid.NewString()
	md.Source = "orders-service"
	md.DataContentType = "application/proto"
	md.Time = time.Now().UTC()
	return md
}
