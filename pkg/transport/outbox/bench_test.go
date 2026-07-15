package outbox_test

import (
	"context"
	"encoding/json"
	"net/url"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/quarks-tech/protoevent-go/pkg/event"
	"github.com/quarks-tech/protoevent-go/pkg/eventbus"
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
		sender := outbox.NewSender(store, outbox.WithRowIDGenerator(outbox.ReuseMetadataID))
		for b.Loop() {
			if err := sender.Send(context.Background(), md, nil); err != nil {
				b.Fatalf("send: %v", err)
			}
		}
	})

	b.Run("GenerateUUIDv4", func(b *testing.B) {
		b.ReportAllocs()
		store := &captureStore{}
		sender := outbox.NewSender(store, outbox.WithRowIDGenerator(outbox.GenerateUUIDv4))
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

// BenchmarkPublisherFactoryPublish measures the real production publish path:
// PublisherFactory.Create (Sender + eventbus.Publisher construction) plus
// Publish (interceptor chain, metadata completion, proto marshal, Send) over
// a no-op Store. The delta vs BenchmarkSenderSend is the eventbus overhead on
// top of the raw outbox write. Two sub-benchmarks isolate Create's own cost:
// "create-per-publish" mirrors the WithTransaction pattern (a fresh publisher
// per business transaction, factory.Create(store) called inside the loop);
// "publisher-reused" builds the publisher once outside the loop.
func BenchmarkPublisherFactoryPublish(b *testing.B) {
	identity := func(p eventbus.Publisher) eventbus.Publisher { return p }

	b.Run("create-per-publish", func(b *testing.B) {
		b.ReportAllocs()
		store := &captureStore{}
		factory := outbox.NewPublisherFactory(identity)
		for b.Loop() {
			publisher := factory.Create(store)
			if err := publisher.Publish(context.Background(), "bench.event", &emptypb.Empty{}); err != nil {
				b.Fatalf("publish: %v", err)
			}
		}
	})

	b.Run("publisher-reused", func(b *testing.B) {
		b.ReportAllocs()
		store := &captureStore{}
		factory := outbox.NewPublisherFactory(identity)
		publisher := factory.Create(store)
		for b.Loop() {
			if err := publisher.Publish(context.Background(), "bench.event", &emptypb.Empty{}); err != nil {
				b.Fatalf("publish: %v", err)
			}
		}
	})
}
