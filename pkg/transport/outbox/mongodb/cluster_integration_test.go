package mongodb_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
	"go.mongodb.org/mongo-driver/v2/mongo/writeconcern"

	"github.com/quarks-tech/protoevent-go/pkg/event"
	"github.com/quarks-tech/protoevent-go/pkg/transport/outbox"
	mongodbstore "github.com/quarks-tech/protoevent-go/pkg/transport/outbox/mongodb"
	"github.com/quarks-tech/protoevent-go/pkg/transport/outbox/mongodb/mongodbtest"
	"github.com/quarks-tech/protoevent-go/pkg/transport/outbox/relay/stream"
)

// TestStrandedMajorityStallsDeliverySilently is the reason the PSA harness exists.
//
// Change streams deliver only MAJORITY-COMMITTED events. In a
// primary-secondary-arbiter set with the secondary down, the primary keeps its vote
// majority and so keeps acknowledging w:1 writes, but the majority commit point needs
// two data-bearing acknowledgements and therefore stops advancing. The publisher sees
// success, the rows are durable on the primary, and the relay's change stream returns
// EMPTY WINDOWS — not errors.
//
// That combination is the dangerous one: the relay reports healthy, the stuck-lane
// escalation never fires (it counts unreadable pages and failed sends, not empty
// windows), and events accumulate undelivered for as long as the secondary is away.
// The only moving signal is the committed-token age.
//
// Note what this test does NOT claim. On a set of three DATA-BEARING members, losing
// the majority also loses the primary — the survivor steps down and writes fail with a
// loud server-selection error. The silent version requires an arbiter to hold the
// primary up, which is why PSA is the topology MongoDB warns about and the one staged
// here.
func TestStrandedMajorityStallsDeliverySilently(t *testing.T) {
	cl, cleanup, err := mongodbtest.StartCluster(context.Background(), mongodbtest.WithArbiter())
	if err != nil {
		if errors.Is(err, mongodbtest.ErrDockerUnavailable) {
			t.Skipf("no Docker: %v", err)
		}
		t.Fatalf("StartCluster: %v", err)
	}
	defer cleanup()

	st := mongodbstore.NewRelayStore(cl.DB)

	// Open the stream while the set is healthy, so nothing about the stall is about
	// the stream failing to open.
	ctx, cancel := context.WithTimeout(t.Context(), 4*time.Minute)
	defer cancel()

	s, err := st.Watch(ctx, "", 500*time.Millisecond)
	if err != nil {
		t.Fatalf("watch: %v", err)
	}
	defer func() {
		cctx, ccancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer ccancel()
		_ = s.Close(cctx)
	}()

	// Baseline: a healthy set delivers.
	healthyID := publishTo(t, cl.DB, "healthy")
	if got := drainIDs(t, ctx, s, 1, 30*time.Second); len(got) != 1 || got[0] != healthyID {
		t.Fatalf("healthy set delivered %v, want [%s]", got, healthyID)
	}

	// Strand the majority commit point: stop the only other data-bearing member.
	cl.StopMember(t, cl.SecondaryIndex(t))

	// The primary must still be writable — that is what makes this failure silent.
	w1 := cl.Client.Database("outbox_test", options.Database().SetWriteConcern(writeconcern.W1()))

	stranded := make([]string, 0, 3)
	for i := range 3 {
		id, err := publishW1(ctx, w1, "stranded")
		if err != nil {
			t.Fatalf("w:1 insert %d with the secondary down: %v (the primary should still accept "+
				"writes in a PSA set — if it does not, this test is no longer staging the silent case)", i, err)
		}
		stranded = append(stranded, id)
	}

	// Now the assertion that matters: several drain windows produce NO events and NO
	// error. Delivery has stopped and nothing says so.
	got := drainIDs(t, ctx, s, len(stranded), 15*time.Second)
	if len(got) != 0 {
		t.Fatalf("the stream delivered %v while the majority commit point was stranded; if MongoDB "+
			"has changed this, the relay's health signals can be simplified", got)
	}

	// Restore the majority and prove the events were never lost — they were waiting.
	cl.StartMember(t, cl.SecondaryIndex(t))
	cl.WaitForPrimary(t)

	got = drainIDs(t, ctx, s, len(stranded), 90*time.Second)
	if len(got) != len(stranded) {
		t.Fatalf("after the majority returned the stream delivered %d of %d stranded events (%v)",
			len(got), len(stranded), got)
	}
	for i, id := range stranded {
		if got[i] != id {
			t.Fatalf("delivery %d = %s, want %s: order was not preserved across the stall", i, got[i], id)
		}
	}
}

// publishTo publishes through the production Sender against an arbitrary database.
func publishTo(t *testing.T, db *mongo.Database, subject string) string {
	t.Helper()

	md := event.NewMetadata("books.created")
	md.ID = uuid.NewString()
	md.Source = "books-service"
	md.Subject = subject
	md.DataContentType = "application/proto"
	md.Time = time.Now().UTC()

	st := mongodbstore.NewStore(db)

	sess, err := db.Client().StartSession()
	if err != nil {
		t.Fatalf("start session: %v", err)
	}
	defer sess.EndSession(context.Background())

	if _, err := sess.WithTransaction(context.Background(), func(sc context.Context) (any, error) {
		return nil, outbox.NewSender(st).Send(sc, md, []byte("x"))
	}); err != nil {
		t.Fatalf("publish: %v", err)
	}

	return md.ID
}

// publishW1 publishes through the production Sender with a session whose commit uses
// w:1, modeling a publisher that does not wait for a majority acknowledgement. Going
// through the real Sender matters: the store owns how metadata is encoded, and a
// hand-built insert would test the test's encoding rather than the store's.
func publishW1(ctx context.Context, db *mongo.Database, subject string) (string, error) {
	md := event.NewMetadata("books.created")
	md.ID = uuid.NewString()
	md.Source = "books-service"
	md.Subject = subject
	md.DataContentType = "application/proto"
	md.Time = time.Now().UTC()

	st := mongodbstore.NewStore(db)

	// The commit's write concern lives on the TRANSACTION options, not the session's
	// own fields, so w:1 is set through the default transaction options.
	sess, err := db.Client().StartSession(
		options.Session().SetDefaultTransactionOptions(
			options.Transaction().SetWriteConcern(writeconcern.W1()),
		),
	)
	if err != nil {
		return "", err
	}
	defer sess.EndSession(ctx)

	if _, err := sess.WithTransaction(ctx, func(sc context.Context) (any, error) {
		return nil, outbox.NewSender(st).Send(sc, md, []byte("x"))
	}); err != nil {
		return "", err
	}

	return md.ID, nil
}

// drainIDs pulls up to want events out of the stream, returning what arrived before
// the timeout. An empty result is a legitimate outcome the caller asserts on.
func drainIDs(t *testing.T, ctx context.Context, s stream.Stream, want int, timeout time.Duration) []string {
	t.Helper()

	var ids []string
	deadline := time.Now().Add(timeout)
	for len(ids) < want && time.Now().Before(deadline) {
		msg, ok, err := s.Next(ctx)
		if err != nil {
			t.Fatalf("stream.Next returned an error where an empty window was expected: %v", err)
		}
		if !ok {
			continue
		}
		ids = append(ids, msg.Message.Metadata.ID)
	}

	return ids
}
