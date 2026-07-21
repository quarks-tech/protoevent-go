package mongodb

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/quarks-tech/protoevent-go/pkg/event"
)

// TestDecodeMessagePoisonShapes pins decodeMessage's poison classification:
// JSON null AND the JSON-valid-but-empty "{}" (non-nil metadata, zero Time —
// rejected by the write side, so corruption by definition) must both error
// instead of going downstream as empty events, while a well-formed document
// still decodes.
func TestDecodeMessagePoisonShapes(t *testing.T) {
	if _, err := decodeMessage(outboxDoc{ID: "e1", Metadata: []byte(`null`)}); err == nil {
		t.Fatal("JSON null metadata decoded without error; want poison classification")
	}
	if _, err := decodeMessage(outboxDoc{ID: "e2", Metadata: []byte(`{}`)}); err == nil {
		t.Fatal("empty-object metadata decoded without error; want poison classification (zero Time)")
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
