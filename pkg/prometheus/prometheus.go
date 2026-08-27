// Package prometheus is the dependency-free-core's ONE opt-in seam onto
// client_golang: a consumer that publishes or subscribes events, or runs an
// outbox relay, imports this module to get RED metrics and relay
// lag/throughput signals; a consumer that does neither never pulls
// client_golang in at all, because this lives in its own go.mod (the same
// reason pkg/transport/rabbitmq is its own module).
//
// Metrics wires three independent seams — eventbus.PublisherInterceptor,
// eventbus.SubscriberInterceptor and relay.Observer — onto one set of
// collectors registered together at New. All three are read-only with
// respect to the pipeline they observe: they record counts, durations and
// gauge values and never alter control flow, an event, or a relay decision.
package prometheus

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/quarks-tech/protoevent-go/pkg/event"
	"github.com/quarks-tech/protoevent-go/pkg/eventbus"
	"github.com/quarks-tech/protoevent-go/pkg/transport/outbox/relay"
)

// defaultNamespace prefixes every metric name (e.g. protoevent_publish_total)
// so a service that registers this alongside its own metrics on the shared
// DefaultRegisterer never collides with them by name alone.
const defaultNamespace = "protoevent"

const (
	resultOK    = "ok"
	resultError = "error"
)

// Metrics holds every collector this package registers. The zero value is
// not usable; construct with New.
type Metrics struct {
	publishTotal    *prometheus.CounterVec
	publishDuration *prometheus.HistogramVec

	handleTotal    *prometheus.CounterVec
	handleDuration *prometheus.HistogramVec
	handleInFlight *prometheus.GaugeVec

	relaySentTotal   *prometheus.CounterVec
	relayLagSeconds  *prometheus.GaugeVec
	relayBacklog     *prometheus.GaugeVec
	relayErrorsTotal *prometheus.CounterVec
	sequencedTotal   *prometheus.CounterVec
	sweptTotal       *prometheus.CounterVec
	sweptLast        *prometheus.GaugeVec
	relayLeader      *prometheus.GaugeVec
}

// Option configures New.
type Option func(*options)

type options struct {
	namespace string
	buckets   []float64
}

// WithNamespace overrides the metric name prefix (default "protoevent").
func WithNamespace(namespace string) Option {
	return func(o *options) { o.namespace = namespace }
}

// WithBuckets overrides the histogram buckets shared by publish_duration_seconds
// and handle_duration_seconds (default prometheus.DefBuckets).
func WithBuckets(buckets []float64) Option {
	return func(o *options) { o.buckets = buckets }
}

// New builds the collector set and registers every one of them on reg. reg is
// required: a nil Registerer is a caller bug (there is no implicit fallback
// to prometheus.DefaultRegisterer here, unlike component/prometheus's
// service-wide default — this package is a library seam, not the thing that
// owns the registry), so New fails fast instead of registering onto nothing.
//
// A registration failure (most commonly: the namespace collides with metrics
// already on reg) returns the error unregistering nothing that already
// succeeded — reg itself is left however client_golang's Register calls left
// it. Callers that need a clean rollback should give New a fresh registry.
func New(reg prometheus.Registerer, opts ...Option) (*Metrics, error) {
	if reg == nil {
		return nil, errors.New("prometheus: New requires a non-nil Registerer")
	}

	o := options{
		namespace: defaultNamespace,
		buckets:   prometheus.DefBuckets,
	}
	for _, opt := range opts {
		opt(&o)
	}

	m := &Metrics{
		publishTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: o.namespace,
			Name:      "publish_total",
			Help:      "Total number of publish attempts, by event type and result.",
		}, []string{"event", "result"}),
		publishDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: o.namespace,
			Name:      "publish_duration_seconds",
			Help:      "Publish latency in seconds, by event type.",
			Buckets:   o.buckets,
		}, []string{"event"}),

		handleTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: o.namespace,
			Name:      "handle_total",
			Help:      "Total number of subscriber handler invocations, by event type and result.",
		}, []string{"event", "result"}),
		handleDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: o.namespace,
			Name:      "handle_duration_seconds",
			Help:      "Subscriber handler latency in seconds, by event type.",
			Buckets:   o.buckets,
		}, []string{"event"}),
		handleInFlight: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: o.namespace,
			Name:      "handle_in_flight",
			Help:      "Number of subscriber handler invocations currently in flight, by event type.",
		}, []string{"event"}),

		relaySentTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: o.namespace,
			Name:      "outbox_relay_sent_total",
			Help:      "Total number of outbox messages sent by the relay, by consumer group.",
		}, []string{"group"}),
		relayLagSeconds: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: o.namespace,
			Name:      "outbox_relay_lag_seconds",
			Help:      "Age of the oldest event in the most recent relay drain, by consumer group.",
		}, []string{"group"}),
		relayBacklog: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: o.namespace,
			Name:      "outbox_relay_backlog",
			Help:      "Whether the relay's most recent drain left more work immediately queued (1) or not (0), by consumer group.",
		}, []string{"group"}),
		relayErrorsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: o.namespace,
			Name:      "outbox_relay_errors_total",
			Help:      "Total number of relay pass-level or per-message errors, by consumer group.",
		}, []string{"group"}),
		sequencedTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: o.namespace,
			Name:      "outbox_sequenced_total",
			Help:      "Total number of rows assigned a sequence number by the sequencer, by consumer group.",
		}, []string{"group"}),
		sweptTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: o.namespace,
			Name:      "outbox_swept_total",
			Help:      "Total number of fully-consumed rows deleted by the retention sweep, by consumer group.",
		}, []string{"group"}),
		sweptLast: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: o.namespace,
			Name:      "outbox_swept_last",
			Help:      "Number of rows deleted by the most recent retention sweep pass, by consumer group. Fired for zero too: a zero sweep while the table keeps growing is the disk-full signal, and it is indistinguishable from a healthy idle sweep by count alone — alert on this gauge, not on outbox_swept_total.",
		}, []string{"group"}),
		relayLeader: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: o.namespace,
			Name:      "outbox_relay_leader",
			Help:      "Whether this relay instance currently holds leadership (1) or not (0), by consumer group. The sum across instances for one group should be 1; anything else is a dual-leader or leaderless episode.",
		}, []string{"group"}),
	}

	collectors := []prometheus.Collector{
		m.publishTotal, m.publishDuration,
		m.handleTotal, m.handleDuration, m.handleInFlight,
		m.relaySentTotal, m.relayLagSeconds, m.relayBacklog, m.relayErrorsTotal,
		m.sequencedTotal, m.sweptTotal, m.sweptLast, m.relayLeader,
	}
	for _, c := range collectors {
		if err := reg.Register(c); err != nil {
			return nil, fmt.Errorf("prometheus: register: %w", err)
		}
	}

	return m, nil
}

// eventType returns md.Type, or "" when md is nil. The subscriber
// interceptor sits ahead of application code in the chain, so a malformed or
// missing envelope must not panic the metrics seam — a blank label is still
// bounded (the catalog of event types plus one blank value), unlike letting
// an arbitrary string through.
func eventType(md *event.Metadata) string {
	if md == nil {
		return ""
	}
	return md.Type
}

func resultLabel(err error) string {
	if err != nil {
		return resultError
	}
	return resultOK
}

func boolToFloat(b bool) float64 {
	if b {
		return 1
	}
	return 0
}

// PublisherInterceptor records publish_total{event,result} and
// publish_duration_seconds{event} around the wrapped PublishFn. It never
// changes the outcome: pf's error, including nil, is returned unchanged.
func (m *Metrics) PublisherInterceptor() eventbus.PublisherInterceptor {
	return func(
		ctx context.Context, name string, e any, p *eventbus.PublisherImpl, pf eventbus.PublishFn, opts ...eventbus.PublishOption,
	) error {
		start := time.Now()
		err := pf(ctx, name, e, p, opts...)

		m.publishTotal.WithLabelValues(name, resultLabel(err)).Inc()
		m.publishDuration.WithLabelValues(name).Observe(time.Since(start).Seconds())

		return err
	}
}

// SubscriberInterceptor records handle_total{event,result} and
// handle_duration_seconds{event}, and tracks handle_in_flight{event} for the
// duration of the wrapped Handler call — incremented before invoking it,
// decremented via defer so a panic recovered upstream still leaves the gauge
// correct. It never changes the outcome: handler's error, including nil, is
// returned unchanged.
func (m *Metrics) SubscriberInterceptor() eventbus.SubscriberInterceptor {
	return func(ctx context.Context, md *event.Metadata, e any, handler eventbus.Handler) error {
		name := eventType(md)

		m.handleInFlight.WithLabelValues(name).Inc()
		defer m.handleInFlight.WithLabelValues(name).Dec()

		start := time.Now()
		err := handler(ctx, e)

		m.handleTotal.WithLabelValues(name, resultLabel(err)).Inc()
		m.handleDuration.WithLabelValues(name).Observe(time.Since(start).Seconds())

		return err
	}
}

// RelayObserver adapts Metrics onto relay.Observer. Every callback only
// touches a counter or gauge — Observer's doc comment (relay/relay.go)
// requires callbacks to never block, because they run on the relay's single
// pass goroutine.
func (m *Metrics) RelayObserver() relay.Observer {
	return relay.Observer{
		OnDrained: func(name string, count int, oldestAge time.Duration, more bool) {
			m.relaySentTotal.WithLabelValues(name).Add(float64(count))
			m.relayLagSeconds.WithLabelValues(name).Set(oldestAge.Seconds())
			m.relayBacklog.WithLabelValues(name).Set(boolToFloat(more))
		},
		OnError: func(name string, _ error) {
			m.relayErrorsTotal.WithLabelValues(name).Inc()
		},
		OnSequenced: func(name string, count int) {
			m.sequencedTotal.WithLabelValues(name).Add(float64(count))
		},
		// OnSwept sets sweptLast unconditionally, including for count == 0: a
		// zero sweep while the table keeps growing is the disk-full signal (see
		// the Help text above and relay.Observer's OnSwept doc), so this must
		// not special-case zero away.
		OnSwept: func(name string, count int) {
			m.sweptTotal.WithLabelValues(name).Add(float64(count))
			m.sweptLast.WithLabelValues(name).Set(float64(count))
		},
		OnLeadership: func(name string, isLeader bool) {
			m.relayLeader.WithLabelValues(name).Set(boolToFloat(isLeader))
		},
	}
}
