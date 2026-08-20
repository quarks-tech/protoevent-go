package rabbitmq_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"

	"github.com/quarks-tech/protoevent-go/pkg/event"
	"github.com/quarks-tech/protoevent-go/pkg/eventbus"
	"github.com/quarks-tech/protoevent-go/pkg/transport/rabbitmq"
)

// TestRequeueBackoffHeadOfLineBlocking measures what the requeue backoff costs a
// consumer whose queue also holds healthy messages.
//
// The consume loop (amqpx drainDeliveries -> consume.Run -> doAcknowledge) is a
// SINGLE serial goroutine: prefetch buys buffering, never concurrency. So the
// backoff sleep in doAcknowledge is not "one prefetch slot held" — it stops the
// whole consumer. This measures the healthy-message rate with and without one
// permanently-failing message in the queue, at two prefetch values.
func TestRequeueBackoffHeadOfLineBlocking(t *testing.T) {
	requireMeasure(t)
	inst := requireBroker(t)

	cases := []struct {
		name     string
		prefetch int
		bad      int
		opts     []rabbitmq.ReceiverOption
	}{
		{name: "baseline/no-failures/prefetch=3", prefetch: 3, bad: 0},
		{name: "1-failing/prefetch=3", prefetch: 3, bad: 1},
		{name: "1-failing/prefetch=20", prefetch: 20, bad: 1},
		{
			name: "1-failing/prefetch=3/backoff-off", prefetch: 3, bad: 1,
			opts: []rabbitmq.ReceiverOption{rabbitmq.WithRequeueBackoff(0, 0)},
		},
	}

	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
			defer cancel()

			var (
				service  = fmt.Sprintf("hol%d.v1", i)
				consumer = fmt.Sprintf("hol%d-consumer", i)
			)
			const healthy = 30

			// Externally-managed quorum topology, as in production.
			inst.DeclareExchange(t, service, "topic")
			inst.DeclareQueue(t, consumer, amqp.Table{
				"x-queue-type":           "quorum",
				"x-delivery-limit":       int32(200),
				"x-max-in-memory-length": int32(0),
			})
			inst.BindQueue(t, consumer, testEventName, service)
			t.Cleanup(func() { inst.DeleteQueue(t, consumer) })

			client := newTestClient(t)
			sd := &eventbus.ServiceDesc{ServiceName: service, Events: []eventbus.EventDesc{{Name: testEventName}}}
			sender := rabbitmq.NewSender(client)
			if err := sender.Setup(ctx, sd); err != nil {
				t.Fatalf("sender setup: %v", err)
			}

			// The failing message goes FIRST, then the healthy ones behind it.
			for range tc.bad {
				md := testEvent(service)
				md.Subject = "bad"
				if err := sender.Send(ctx, md, []byte("p")); err != nil {
					t.Fatalf("send bad: %v", err)
				}
			}
			for range healthy {
				md := testEvent(service)
				md.Subject = "ok"
				if err := sender.Send(ctx, md, []byte("p")); err != nil {
					t.Fatalf("send ok: %v", err)
				}
			}

			var (
				mu       sync.Mutex
				okSeen   int
				badTries int
				start    time.Time
			)
			allOK := make(chan struct{})
			var once sync.Once

			sub := eventbus.NewSubscriber(consumer)
			sub.RegisterHandler(sd, testEventName,
				func(_ context.Context, md *event.Metadata, _ func(any) error, _ eventbus.SubscriberInterceptor) error {
					mu.Lock()
					if start.IsZero() {
						start = time.Now()
					}
					if md.Subject == "bad" {
						badTries++
						mu.Unlock()

						return errors.New("downstream 503")
					}
					okSeen++
					done := okSeen >= healthy
					mu.Unlock()
					if done {
						once.Do(func() { close(allOK) })
					}

					return nil
				})

			opts := append([]rabbitmq.ReceiverOption{
				rabbitmq.WithIncomingQueue(consumer),
				rabbitmq.WithPrefetchCount(tc.prefetch),
			}, tc.opts...)
			receiver := rabbitmq.NewReceiver(client, opts...)

			subCtx, stopSub := context.WithCancel(ctx)
			done := make(chan error, 1)
			go func() { done <- sub.Subscribe(subCtx, receiver) }()

			// Measurement window: however long the healthy messages take, capped.
			const window = 20 * time.Second
			var elapsed time.Duration
			select {
			case <-allOK:
				mu.Lock()
				elapsed = time.Since(start)
				mu.Unlock()
			case <-time.After(window):
				elapsed = window
			}

			stopSub()
			<-done

			mu.Lock()
			t.Logf("prefetch=%d bad=%d: %d/%d healthy handled in %v = %.1f/s (bad redelivered %d times)",
				tc.prefetch, tc.bad, okSeen, healthy, elapsed.Round(time.Millisecond),
				float64(okSeen)/elapsed.Seconds(), badTries)
			mu.Unlock()
		})
	}
}
