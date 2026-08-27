package prometheus_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/quarks-tech/protoevent-go/pkg/event"
	"github.com/quarks-tech/protoevent-go/pkg/eventbus"
	protoprom "github.com/quarks-tech/protoevent-go/pkg/prometheus"
)

func TestNew_NilRegisterer(t *testing.T) {
	m, err := protoprom.New(nil)
	if err == nil {
		t.Fatal("New(nil) = nil error, want an error")
	}
	if m != nil {
		t.Fatal("New(nil) returned a non-nil *Metrics alongside an error")
	}
}

func TestNew_TwoNamespacesOnOneRegistryDoNotCollide(t *testing.T) {
	reg := prometheus.NewPedanticRegistry()

	if _, err := protoprom.New(reg, protoprom.WithNamespace("a")); err != nil {
		t.Fatalf("New(namespace=a) error: %v", err)
	}
	if _, err := protoprom.New(reg, protoprom.WithNamespace("b")); err != nil {
		t.Fatalf("New(namespace=b) error: %v", err)
	}
}

func TestNew_SameNamespaceTwiceCollides(t *testing.T) {
	reg := prometheus.NewPedanticRegistry()

	if _, err := protoprom.New(reg); err != nil {
		t.Fatalf("first New() error: %v", err)
	}
	if _, err := protoprom.New(reg); err == nil {
		t.Fatal("second New() with the same default namespace on the same registry = nil error, want a registration collision")
	}
}

func TestPublisherInterceptor(t *testing.T) {
	reg := prometheus.NewPedanticRegistry()

	m, err := protoprom.New(reg)
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}

	interceptor := m.PublisherInterceptor()

	okFn := func(_ context.Context, _ string, _ any, _ *eventbus.PublisherImpl, _ ...eventbus.PublishOption) error {
		return nil
	}
	errFn := func(_ context.Context, _ string, _ any, _ *eventbus.PublisherImpl, _ ...eventbus.PublishOption) error {
		return errors.New("boom")
	}

	if err := interceptor(context.Background(), "order.created", nil, nil, okFn); err != nil {
		t.Fatalf("interceptor (ok) returned error: %v", err)
	}
	if err := interceptor(context.Background(), "order.created", nil, nil, errFn); err == nil {
		t.Fatal("interceptor (error) returned nil error, want the wrapped failure")
	}

	expected := `
# HELP protoevent_publish_total Total number of publish attempts, by event type and result.
# TYPE protoevent_publish_total counter
protoevent_publish_total{event="order.created",result="error"} 1
protoevent_publish_total{event="order.created",result="ok"} 1
`
	if err := testutil.GatherAndCompare(reg, strings.NewReader(expected), "protoevent_publish_total"); err != nil {
		t.Fatal(err)
	}

	// The histogram is not compared bucket-by-bucket (that couples the test to
	// bucket boundaries, not behavior) — just confirm one series exists per
	// event, i.e. that PublisherInterceptor observed a duration at all.
	n, err := testutil.GatherAndCount(reg, "protoevent_publish_duration_seconds")
	if err != nil {
		t.Fatalf("GatherAndCount error: %v", err)
	}
	if n != 1 {
		t.Fatalf("protoevent_publish_duration_seconds series count = %d, want 1", n)
	}
}

func TestSubscriberInterceptor_InFlightReturnsToZero(t *testing.T) {
	reg := prometheus.NewPedanticRegistry()

	m, err := protoprom.New(reg)
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}

	interceptor := m.SubscriberInterceptor()
	md := &event.Metadata{Type: "order.created"}

	okHandler := func(_ context.Context, _ any) error { return nil }
	errHandler := func(_ context.Context, _ any) error { return errors.New("boom") }

	if err := interceptor(context.Background(), md, nil, okHandler); err != nil {
		t.Fatalf("interceptor (ok) returned error: %v", err)
	}

	inFlightExpectedZero := `
# HELP protoevent_handle_in_flight Number of subscriber handler invocations currently in flight, by event type.
# TYPE protoevent_handle_in_flight gauge
protoevent_handle_in_flight{event="order.created"} 0
`
	if err := testutil.GatherAndCompare(reg, strings.NewReader(inFlightExpectedZero), "protoevent_handle_in_flight"); err != nil {
		t.Fatalf("in-flight after ok handler: %v", err)
	}

	if err := interceptor(context.Background(), md, nil, errHandler); err == nil {
		t.Fatal("interceptor (error) returned nil error, want the handler failure")
	}
	if err := testutil.GatherAndCompare(reg, strings.NewReader(inFlightExpectedZero), "protoevent_handle_in_flight"); err != nil {
		t.Fatalf("in-flight after error handler: %v", err)
	}

	totalExpected := `
# HELP protoevent_handle_total Total number of subscriber handler invocations, by event type and result.
# TYPE protoevent_handle_total counter
protoevent_handle_total{event="order.created",result="error"} 1
protoevent_handle_total{event="order.created",result="ok"} 1
`
	if err := testutil.GatherAndCompare(reg, strings.NewReader(totalExpected), "protoevent_handle_total"); err != nil {
		t.Fatal(err)
	}
}

func TestSubscriberInterceptor_NilMetadataIsSafe(t *testing.T) {
	reg := prometheus.NewPedanticRegistry()

	m, err := protoprom.New(reg)
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}

	interceptor := m.SubscriberInterceptor()

	if err := interceptor(context.Background(), nil, nil, func(_ context.Context, _ any) error { return nil }); err != nil {
		t.Fatalf("interceptor with nil metadata returned error: %v", err)
	}

	expected := `
# HELP protoevent_handle_total Total number of subscriber handler invocations, by event type and result.
# TYPE protoevent_handle_total counter
protoevent_handle_total{event="",result="ok"} 1
`
	if err := testutil.GatherAndCompare(reg, strings.NewReader(expected), "protoevent_handle_total"); err != nil {
		t.Fatal(err)
	}
}

func TestRelayObserver_OnDrained(t *testing.T) {
	reg := prometheus.NewPedanticRegistry()

	m, err := protoprom.New(reg)
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}

	obs := m.RelayObserver()
	obs.OnDrained("g", 5, 3*time.Second, true)

	expected := `
# HELP protoevent_outbox_relay_sent_total Total number of outbox messages sent by the relay, by consumer group.
# TYPE protoevent_outbox_relay_sent_total counter
protoevent_outbox_relay_sent_total{group="g"} 5
# HELP protoevent_outbox_relay_lag_seconds Age of the oldest event in the most recent relay drain, by consumer group.
# TYPE protoevent_outbox_relay_lag_seconds gauge
protoevent_outbox_relay_lag_seconds{group="g"} 3
# HELP protoevent_outbox_relay_backlog Whether the relay's most recent drain left more work immediately queued (1) or not (0), by consumer group.
# TYPE protoevent_outbox_relay_backlog gauge
protoevent_outbox_relay_backlog{group="g"} 1
`
	if err := testutil.GatherAndCompare(reg, strings.NewReader(expected),
		"protoevent_outbox_relay_sent_total", "protoevent_outbox_relay_lag_seconds", "protoevent_outbox_relay_backlog",
	); err != nil {
		t.Fatal(err)
	}

	obs.OnDrained("g", 2, time.Second, false)

	expected = `
# HELP protoevent_outbox_relay_sent_total Total number of outbox messages sent by the relay, by consumer group.
# TYPE protoevent_outbox_relay_sent_total counter
protoevent_outbox_relay_sent_total{group="g"} 7
# HELP protoevent_outbox_relay_backlog Whether the relay's most recent drain left more work immediately queued (1) or not (0), by consumer group.
# TYPE protoevent_outbox_relay_backlog gauge
protoevent_outbox_relay_backlog{group="g"} 0
`
	if err := testutil.GatherAndCompare(reg, strings.NewReader(expected),
		"protoevent_outbox_relay_sent_total", "protoevent_outbox_relay_backlog",
	); err != nil {
		t.Fatal(err)
	}
}

func TestRelayObserver_OnError(t *testing.T) {
	reg := prometheus.NewPedanticRegistry()

	m, err := protoprom.New(reg)
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}

	obs := m.RelayObserver()
	obs.OnError("g", errors.New("boom"))
	obs.OnError("g", errors.New("boom again"))

	expected := `
# HELP protoevent_outbox_relay_errors_total Total number of relay pass-level or per-message errors, by consumer group.
# TYPE protoevent_outbox_relay_errors_total counter
protoevent_outbox_relay_errors_total{group="g"} 2
`
	if err := testutil.GatherAndCompare(reg, strings.NewReader(expected), "protoevent_outbox_relay_errors_total"); err != nil {
		t.Fatal(err)
	}
}

func TestRelayObserver_OnSequenced(t *testing.T) {
	reg := prometheus.NewPedanticRegistry()

	m, err := protoprom.New(reg)
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}

	obs := m.RelayObserver()
	obs.OnSequenced("g", 4)
	obs.OnSequenced("g", 6)

	expected := `
# HELP protoevent_outbox_sequenced_total Total number of rows assigned a sequence number by the sequencer, by consumer group.
# TYPE protoevent_outbox_sequenced_total counter
protoevent_outbox_sequenced_total{group="g"} 10
`
	if err := testutil.GatherAndCompare(reg, strings.NewReader(expected), "protoevent_outbox_sequenced_total"); err != nil {
		t.Fatal(err)
	}
}

func TestRelayObserver_OnSwept(t *testing.T) {
	reg := prometheus.NewPedanticRegistry()

	m, err := protoprom.New(reg)
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}

	obs := m.RelayObserver()

	// A zero sweep must still move the gauge (it is the disk-full signal) but
	// must NOT move the counter.
	obs.OnSwept("g", 0)

	expected := `
# HELP protoevent_outbox_swept_last Number of rows deleted by the most recent retention sweep pass, by consumer group. Fired for zero too: a zero sweep while the table keeps growing is the disk-full signal, and it is indistinguishable from a healthy idle sweep by count alone — alert on this gauge, not on outbox_swept_total.
# TYPE protoevent_outbox_swept_last gauge
protoevent_outbox_swept_last{group="g"} 0
# HELP protoevent_outbox_swept_total Total number of fully-consumed rows deleted by the retention sweep, by consumer group.
# TYPE protoevent_outbox_swept_total counter
protoevent_outbox_swept_total{group="g"} 0
`
	if err := testutil.GatherAndCompare(reg, strings.NewReader(expected),
		"protoevent_outbox_swept_last", "protoevent_outbox_swept_total",
	); err != nil {
		t.Fatal(err)
	}

	obs.OnSwept("g", 7)

	expected = `
# HELP protoevent_outbox_swept_last Number of rows deleted by the most recent retention sweep pass, by consumer group. Fired for zero too: a zero sweep while the table keeps growing is the disk-full signal, and it is indistinguishable from a healthy idle sweep by count alone — alert on this gauge, not on outbox_swept_total.
# TYPE protoevent_outbox_swept_last gauge
protoevent_outbox_swept_last{group="g"} 7
# HELP protoevent_outbox_swept_total Total number of fully-consumed rows deleted by the retention sweep, by consumer group.
# TYPE protoevent_outbox_swept_total counter
protoevent_outbox_swept_total{group="g"} 7
`
	if err := testutil.GatherAndCompare(reg, strings.NewReader(expected),
		"protoevent_outbox_swept_last", "protoevent_outbox_swept_total",
	); err != nil {
		t.Fatal(err)
	}
}

func TestRelayObserver_OnLeadership(t *testing.T) {
	reg := prometheus.NewPedanticRegistry()

	m, err := protoprom.New(reg)
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}

	obs := m.RelayObserver()

	obs.OnLeadership("g", true)
	expected := `
# HELP protoevent_outbox_relay_leader Whether this relay instance currently holds leadership (1) or not (0), by consumer group. The sum across instances for one group should be 1; anything else is a dual-leader or leaderless episode.
# TYPE protoevent_outbox_relay_leader gauge
protoevent_outbox_relay_leader{group="g"} 1
`
	if err := testutil.GatherAndCompare(reg, strings.NewReader(expected), "protoevent_outbox_relay_leader"); err != nil {
		t.Fatal(err)
	}

	obs.OnLeadership("g", false)
	expected = `
# HELP protoevent_outbox_relay_leader Whether this relay instance currently holds leadership (1) or not (0), by consumer group. The sum across instances for one group should be 1; anything else is a dual-leader or leaderless episode.
# TYPE protoevent_outbox_relay_leader gauge
protoevent_outbox_relay_leader{group="g"} 0
`
	if err := testutil.GatherAndCompare(reg, strings.NewReader(expected), "protoevent_outbox_relay_leader"); err != nil {
		t.Fatal(err)
	}
}
