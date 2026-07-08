package outbox_test

import (
	"context"
	"encoding/json"
	"net/url"
	"testing"
	"time"

	"github.com/quarks-tech/protoevent-go/pkg/event"
	"github.com/quarks-tech/protoevent-go/pkg/transport/outbox"
)

// BenchmarkSenderSend measures Sender.Send over a no-op in-memory Store: ID
// generation + Message construction overhead, not real persistence. The two
// sub-benchmarks isolate the cost of the two built-in IDGenerators.
func BenchmarkSenderSend(b *testing.B) {
	md := newMetadata("bench-event-id")

	b.Run("ReuseMetadataID", func(b *testing.B) {
		b.ReportAllocs()
		store := &captureStore{}
		sender := outbox.NewSender(store, outbox.WithIDGenerator(outbox.ReuseMetadataID))
		for b.Loop() {
			if err := sender.Send(context.Background(), md, nil); err != nil {
				b.Fatalf("send: %v", err)
			}
		}
	})

	b.Run("GenerateV4", func(b *testing.B) {
		b.ReportAllocs()
		store := &captureStore{}
		sender := outbox.NewSender(store, outbox.WithIDGenerator(outbox.GenerateV4))
		for b.Loop() {
			if err := sender.Send(context.Background(), md, nil); err != nil {
				b.Fatalf("send: %v", err)
			}
		}
	})
}

// BenchmarkMetadataJSONRoundTrip approximates the per-event serialization
// cost both the tidb and mongodb stores pay: json.Marshal then
// json.Unmarshal of a representative CloudEvents envelope with extensions
// and a DataSchema set.
func BenchmarkMetadataJSONRoundTrip(b *testing.B) {
	b.ReportAllocs()

	schema, err := url.Parse("https://schemas.example.com/books/created/v1")
	if err != nil {
		b.Fatalf("parse schema: %v", err)
	}
	md := event.NewMetadata("books.created")
	md.ID = "11111111-1111-4111-8111-111111111111"
	md.Source = "books-service"
	md.Subject = "bench"
	md.DataContentType = "application/proto"
	md.Time = time.Now().UTC()
	md.DataSchema = schema
	md.Extensions = map[string]any{
		"partitionkey": "k1",
		"traceparent":  "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01",
	}

	for b.Loop() {
		data, err := json.Marshal(md)
		if err != nil {
			b.Fatalf("marshal: %v", err)
		}
		var out event.Metadata
		if err := json.Unmarshal(data, &out); err != nil {
			b.Fatalf("unmarshal: %v", err)
		}
	}
}
