package mongodb

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/quarks-tech/protoevent-go/pkg/event"
	"github.com/quarks-tech/protoevent-go/pkg/transport/outbox"
)

// TestDecodeMessagePoisonShapes pins decodeMessage's poison classification:
// JSON null AND the JSON-valid-but-empty "{}" (non-nil metadata, zero Time —
// rejected by the write side, so corruption by definition) must both error
// instead of going downstream as empty events, while a well-formed document
// still decodes.
//
// The errors must wrap outbox.ErrPoisonEnvelope: the classification is the
// SHARED envelope contract (outbox.UnmarshalMetadata), not a mongo-local rule,
// so a row this store calls poison is poison on every backend.
func TestDecodeMessagePoisonShapes(t *testing.T) {
	if _, err := decodeMessage(outboxDoc{ID: "e1", Metadata: []byte(`null`)}); !errors.Is(err, outbox.ErrPoisonEnvelope) {
		t.Fatalf("JSON null metadata: err = %v, want one wrapping outbox.ErrPoisonEnvelope", err)
	}
	if _, err := decodeMessage(outboxDoc{ID: "e2", Metadata: []byte(`{}`)}); !errors.Is(err, outbox.ErrPoisonEnvelope) {
		t.Fatalf("empty-object metadata: err = %v, want one wrapping outbox.ErrPoisonEnvelope (zero Time)", err)
	}

	md := event.NewMetadata("books.created")
	md.ID = "e3"
	md.Time = time.Now().UTC()
	good, err := json.Marshal(md)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	msg, err := decodeMessage(outboxDoc{ID: "e3", Metadata: good, CreateTime: md.Time})
	if err != nil {
		t.Fatalf("well-formed metadata rejected: %v", err)
	}
	if msg.Metadata.ID != "e3" {
		t.Fatalf("decoded Metadata.ID = %q, want e3", msg.Metadata.ID)
	}
}
