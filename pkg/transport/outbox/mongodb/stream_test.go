package mongodb_test

import (
	"context"
	"net/url"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/quarks-tech/protoevent-go/pkg/event"
	mongodbstore "github.com/quarks-tech/protoevent-go/pkg/transport/outbox/mongodb"
)

func TestWatchDeliversInsertsInOrder(t *testing.T) {
	if testDB == nil {
		t.Skip("no MongoDB")
	}
	reset(t)
	st := mongodbstore.NewStore(testDB, mongodbstore.WithMaxAwaitTime(300*time.Millisecond))

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	// Open the stream at "now" BEFORE publishing, so we catch the inserts.
	strm, err := st.Watch(ctx, "")
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
	st := mongodbstore.NewStore(testDB, mongodbstore.WithMaxAwaitTime(300*time.Millisecond))
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	s1, err := st.Watch(ctx, "")
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
	s2, err := st.Watch(ctx, tok)
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
	st := mongodbstore.NewStore(testDB, mongodbstore.WithMaxAwaitTime(300*time.Millisecond))
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	// A fresh open (token == "") must surface a resumable baseline token from
	// the initial aggregate's postBatchResumeToken BEFORE any Next/TryNext
	// call — this is what the stream relay's fresh-group baseline persist
	// (relay/stream/run.go) depends on to avoid silently skipping a first
	// event that fails to send under stop-the-lane.
	s, err := st.Watch(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close(context.Background()) }()

	tok, ct := s.PBRT()
	if tok == "" {
		t.Fatal("PBRT returned empty immediately after Watch (before any Next); the fresh-group baseline persist relies on this being non-empty")
	}
	if ct.IsZero() {
		t.Fatal("PBRT returned a zero clusterTime immediately after Watch")
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
	st := mongodbstore.NewStore(testDB, mongodbstore.WithMaxAwaitTime(300*time.Millisecond))

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	strm, err := st.Watch(ctx, "")
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
	md.Extensions = map[string]interface{}{"partitionkey": "k1"}
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

func TestPBRTAdvancesOnIdle(t *testing.T) {
	if testDB == nil {
		t.Skip("no MongoDB")
	}
	reset(t)
	st := mongodbstore.NewStore(testDB, mongodbstore.WithMaxAwaitTime(300*time.Millisecond))
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	s, err := st.Watch(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close(context.Background()) }()

	// Idle: no matching inserts. An empty window must yield a non-nil PBRT.
	_, ok, err := s.Next(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("unexpected event on an idle stream")
	}
	tok, _ := s.PBRT()
	if tok == "" {
		t.Fatal("PBRT returned empty on an empty window; a caught-up consumer would fall off the oplog")
	}
}
