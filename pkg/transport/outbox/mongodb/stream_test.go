package mongodb_test

import (
	"context"
	"errors"
	"net/url"
	"testing"
	"time"

	"github.com/google/uuid"
	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/quarks-tech/protoevent-go/pkg/event"
	mongodbstore "github.com/quarks-tech/protoevent-go/pkg/transport/outbox/mongodb"
	"github.com/quarks-tech/protoevent-go/pkg/transport/outbox/relay/stream"
)

// testMaxAwait is the change stream maxAwaitTime the tests pass to Watch — in
// production the relay passes its DrainWindow here (the single latency knob).
const testMaxAwait = 300 * time.Millisecond

func TestWatchDeliversInsertsInOrder(t *testing.T) {
	if testDB == nil {
		t.Skip("no MongoDB")
	}
	reset(t)
	st := mongodbstore.NewStore(testDB)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	// Open the stream at "now" BEFORE publishing, so we catch the inserts.
	strm, err := st.Watch(ctx, "", testMaxAwait)
	if err != nil {
		t.Fatalf("watch: %v", err)
	}
	defer func() { _ = strm.Close(context.Background()) }()

	ids := []string{publish(t, "a"), publish(t, "b"), publish(t, "c")}

	var got []string
	for len(got) < 3 {
		e, ok, err := strm.Next(ctx)
		if err != nil {
			t.Fatalf("next: %v", err)
		}
		if !ok {
			continue // empty window; keep waiting
		}
		got = append(got, e.Message.ID)
	}
	for i := range ids {
		if got[i] != ids[i] {
			t.Fatalf("order[%d] = %s, want %s (commit order)", i, got[i], ids[i])
		}
	}
}

func TestWatchResumesFromToken(t *testing.T) {
	if testDB == nil {
		t.Skip("no MongoDB")
	}
	reset(t)
	st := mongodbstore.NewStore(testDB)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	s1, err := st.Watch(ctx, "", testMaxAwait)
	if err != nil {
		t.Fatal(err)
	}
	first := publish(t, "first")
	_ = publish(t, "second")

	// consume only the first, capture its token, close.
	var tok string
	for {
		e, ok, err := s1.Next(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if !ok {
			continue
		}
		if e.Message.ID == first {
			tok = e.ResumeToken
			break
		}
	}
	_ = s1.Close(context.Background())

	// resume from the token: must NOT re-deliver "first".
	s2, err := st.Watch(ctx, tok, testMaxAwait)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s2.Close(context.Background()) }()
	e, ok, err := s2.Next(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if ok && e.Message.ID == first {
		t.Fatal("resume re-delivered the already-consumed event")
	}
}

func TestPBRTNonEmptyImmediatelyAfterWatch(t *testing.T) {
	if testDB == nil {
		t.Skip("no MongoDB")
	}
	reset(t)
	st := mongodbstore.NewStore(testDB)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	// A fresh open (token == "") must surface a resumable baseline token from
	// the initial aggregate's postBatchResumeToken BEFORE any Next/TryNext
	// call — this is what the stream relay's fresh-group baseline persist
	// (relay/stream/run.go) depends on to avoid silently skipping a first
	// event that fails to send under stop-the-lane.
	s, err := st.Watch(ctx, "", testMaxAwait)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close(context.Background()) }()

	tok, ct, serverTime := s.Checkpoint()
	if tok == "" {
		t.Fatal("Checkpoint returned empty immediately after Watch (before any Next); the fresh-group baseline persist relies on this being non-empty")
	}
	if !serverTime {
		t.Error("Checkpoint reported serverTime=false for a real postBatchResumeToken: the relay would refuse to calibrate its clock offset and fall back to the raw host clock")
	}
	if ct.IsZero() {
		t.Fatal("Checkpoint returned a zero clusterTime immediately after Watch")
	}
}

// TestMetadataFullFidelityRoundTrip proves the mongo store round-trips the
// FULL CloudEvents envelope — Extensions and DataSchema in particular — the
// same way the TiDB store now does (pkg/transport/outbox/tidb), so both
// backends honor the same envelope contract. Read back through the change
// stream, since decodeMessage is package-internal.
func TestMetadataFullFidelityRoundTrip(t *testing.T) {
	if testDB == nil {
		t.Skip("no MongoDB")
	}
	reset(t)
	st := mongodbstore.NewStore(testDB)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	strm, err := st.Watch(ctx, "", testMaxAwait)
	if err != nil {
		t.Fatalf("watch: %v", err)
	}
	defer func() { _ = strm.Close(context.Background()) }()

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

	id := publishMetadata(t, md, []byte("payload"))

	var got *event.Metadata
	for got == nil {
		e, ok, err := strm.Next(ctx)
		if err != nil {
			t.Fatalf("next: %v", err)
		}
		if !ok {
			continue
		}
		if e.Message.ID == id {
			got = e.Message.Metadata
		}
	}

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

// TestWatchSurfacesInvalidateOnDrop proves that dropping the outbox
// collection surfaces stream.ErrInvalidated through Next, instead of
// silently producing empty windows forever. Verified live (see the F7
// report): with the pipeline matching ONLY "insert", the driver's invalidate
// event was filtered out before it ever reached the caller — TryNext just
// kept returning false with no error, an unrecoverable-but-silent condition.
// The fix widens the $match to insert-or-invalidate.
func TestWatchSurfacesInvalidateOnDrop(t *testing.T) {
	if testDB == nil {
		t.Skip("no MongoDB")
	}
	reset(t)
	st := mongodbstore.NewStore(testDB)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	strm, err := st.Watch(ctx, "", testMaxAwait)
	if err != nil {
		t.Fatalf("watch: %v", err)
	}
	defer func() { _ = strm.Close(context.Background()) }()

	_ = publish(t, "before-drop")

	if err := testDB.Collection("outbox_messages").Drop(ctx); err != nil {
		t.Fatalf("drop outbox: %v", err)
	}

	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		_, _, err := strm.Next(ctx)
		if errors.Is(err, stream.ErrInvalidated) {
			return // the claim: invalidate must surface as the sentinel
		}
		if err != nil {
			t.Fatalf("next returned an unexpected error instead of ErrInvalidated: %v", err)
		}
	}
	t.Fatal("drop of the outbox collection never surfaced ErrInvalidated")
}

// TestWatchJSONNullMetadataIsPoison is the regression test for the JSON-null
// bypass: `null` unmarshals into a struct VALUE without error (leaving it
// zero), so before the pointer-target fix a metadata document holding the
// literal bytes "null" sailed past decodeMessage and went downstream as an
// empty event instead of a *stream.DecodeError.
func TestWatchJSONNullMetadataIsPoison(t *testing.T) {
	if testDB == nil {
		t.Skip("no MongoDB")
	}
	reset(t)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	strm, err := mongodbstore.NewStore(testDB).Watch(ctx, "", testMaxAwait)
	if err != nil {
		t.Fatalf("watch: %v", err)
	}
	defer func() { _ = strm.Close(context.Background()) }()

	if _, err := testDB.Collection("outbox_messages").InsertOne(ctx, bson.M{
		"_id":         "null-md",
		"metadata":    []byte("null"),
		"data":        []byte("x"),
		"create_time": time.Now().UTC(),
	}); err != nil {
		t.Fatalf("insert null-metadata doc: %v", err)
	}

	var de *stream.DecodeError
	for de == nil {
		e, ok, err := strm.Next(ctx)
		if err != nil {
			d, isPoison := errors.AsType[*stream.DecodeError](err)
			if !isPoison {
				t.Fatalf("next returned %T (%v), want *stream.DecodeError (JSON null must classify as poison)", err, err)
			}
			de = d
			continue
		}
		if ok {
			t.Fatalf("JSON-null metadata delivered as event %s instead of surfacing as poison", e.Message.ID)
		}
	}
	if de.ID != "null-md" || de.ResumeToken == "" {
		t.Fatalf("DecodeError = {ID:%q, token empty:%v}, want ID null-md with a resume token", de.ID, de.ResumeToken == "")
	}
}

// TestWatchPoisonEventReturnsDecodeError proves an undecodable outbox row
// surfaces as *stream.DecodeError carrying the event's resume position —
// NOT a bare error, which would wedge the lane forever (reopen from the last
// saved token → same poison event → same error). With the position in hand,
// the relay's PoisonHandler can park the event and a subsequent Watch from
// DecodeError.ResumeToken resumes PAST it.
func TestWatchPoisonEventReturnsDecodeError(t *testing.T) {
	if testDB == nil {
		t.Skip("no MongoDB")
	}
	reset(t)
	st := mongodbstore.NewStore(testDB)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	strm, err := st.Watch(ctx, "", testMaxAwait)
	if err != nil {
		t.Fatalf("watch: %v", err)
	}
	defer func() { _ = strm.Close(context.Background()) }()

	// A raw driver insert bypasses CreateOutboxMessage's marshaling: metadata
	// is stored as bytes that are NOT valid CloudEvents JSON, so decodeMessage
	// must fail on this event.
	if _, err := testDB.Collection("outbox_messages").InsertOne(ctx, bson.M{
		"_id":         "poison",
		"metadata":    []byte("{corrupt"),
		"data":        []byte("x"),
		"create_time": time.Now().UTC(),
	}); err != nil {
		t.Fatalf("insert poison doc: %v", err)
	}
	good := publish(t, "after-poison")

	var de *stream.DecodeError
	for de == nil {
		e, ok, err := strm.Next(ctx)
		if err != nil {
			d, isPoison := errors.AsType[*stream.DecodeError](err)
			if !isPoison {
				t.Fatalf("next returned %T (%v), want *stream.DecodeError", err, err)
			}
			de = d
			continue
		}
		if ok {
			t.Fatalf("delivered event %s before surfacing the poison decode error", e.Message.ID)
		}
	}
	if de.ID != "poison" {
		t.Fatalf("DecodeError.ID = %q, want poison", de.ID)
	}
	if de.ResumeToken == "" {
		t.Fatal("DecodeError.ResumeToken is empty; the relay could not resume past the poison event")
	}
	if de.CommitTime.IsZero() {
		t.Fatal("DecodeError.CommitTime is zero")
	}

	// Resume from the poison event's token: it must be skipped, and the next
	// delivery must be the good event published after it.
	s2, err := st.Watch(ctx, de.ResumeToken, testMaxAwait)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s2.Close(context.Background()) }()
	for {
		e, ok, err := s2.Next(ctx)
		if err != nil {
			t.Fatalf("resumed next: %v", err)
		}
		if !ok {
			continue
		}
		if e.Message.ID == "poison" {
			t.Fatal("resume from DecodeError.ResumeToken re-delivered the poison event")
		}
		if e.Message.ID == good {
			return // poison skipped, stream moved on
		}
	}
}

func TestPBRTAdvancesOnIdle(t *testing.T) {
	if testDB == nil {
		t.Skip("no MongoDB")
	}
	reset(t)
	st := mongodbstore.NewStore(testDB)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	s, err := st.Watch(ctx, "", testMaxAwait)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close(context.Background()) }()

	// Idle: no matching inserts. An empty window must yield a non-nil Checkpoint.
	_, ok, err := s.Next(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("unexpected event on an idle stream")
	}
	tok, _, _ := s.Checkpoint()
	if tok == "" {
		t.Fatal("Checkpoint returned empty on an empty window; a caught-up consumer would fall off the oplog")
	}
}
