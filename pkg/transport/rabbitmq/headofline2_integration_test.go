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

// holSetup declares an externally-managed quorum queue with the given delivery
// limit and returns the service/queue names.
func holSetup(t *testing.T, tag string, limit int32) (service, consumer string) {
	t.Helper()
	inst := requireBroker(t)
	service = tag + ".v1"
	consumer = tag + "-consumer"
	inst.DeclareExchange(t, service, "topic")
	inst.DeclareQueue(t, consumer, amqp.Table{
		"x-queue-type":     "quorum",
		"x-delivery-limit": limit,
	})
	inst.BindQueue(t, consumer, testEventName, service)
	t.Cleanup(func() { inst.DeleteQueue(t, consumer) })

	return service, consumer
}

func holPublish(t *testing.T, ctx context.Context, service string, subjects []string) {
	t.Helper()
	client := newTestClient(t)
	sd := &eventbus.ServiceDesc{ServiceName: service, Events: []eventbus.EventDesc{{Name: testEventName}}}
	s := rabbitmq.NewSender(client)
	if err := s.Setup(ctx, sd); err != nil {
		t.Fatalf("sender setup: %v", err)
	}
	for _, subj := range subjects {
		md := testEvent(service)
		md.Subject = subj
		if err := s.Send(ctx, md, []byte("p")); err != nil {
			t.Fatalf("send %q: %v", subj, err)
		}
	}
}

// TestOnePoisonMessageStallsTheConsumer measures how long ONE permanently-failing
// message blocks a whole consumer at the DEFAULT prefetch, until the quorum
// queue's x-delivery-limit finally discards it.
//
// This is the cost of the paced requeue: the pacing sleep runs inline on the
// single consume goroutine, so the delay is not paid by the failing message — it
// is paid by everything behind it.
func TestOnePoisonMessageStallsTheConsumer(t *testing.T) {
	requireMeasure(t)
	t.Run("paced (default)", func(t *testing.T) { onePoisonRun(t, "poisonpaced", nil) })
	// Control: the SAME scenario with pacing disabled. If this clears in
	// milliseconds, the stall above is the pacing sleep and nothing else.
	t.Run("pacing disabled", func(t *testing.T) {
		onePoisonRun(t, "poisonraw", []rabbitmq.ReceiverOption{rabbitmq.WithRequeueBackoff(0, 0)})
	})
}

func onePoisonRun(t *testing.T, tag string, extra []rabbitmq.ReceiverOption) {
	t.Helper()

	ctx, cancel := context.WithTimeout(t.Context(), 8*time.Minute)
	defer cancel()

	// 20 is RabbitMQ 4.x's default x-delivery-limit for a quorum queue.
	service, consumer := holSetup(t, tag, 20)

	const healthy = 20
	subjects := make([]string, 0, 1+healthy)
	subjects = append(subjects, "bad")
	for range healthy {
		subjects = append(subjects, "ok")
	}
	holPublish(t, ctx, service, subjects)

	var (
		mu       sync.Mutex
		okSeen   int
		badTries int
		start    time.Time
		marks    []time.Duration
	)
	allOK := make(chan struct{})
	var once sync.Once

	sd := &eventbus.ServiceDesc{ServiceName: service, Events: []eventbus.EventDesc{{Name: testEventName}}}
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

				return errors.New("handler bug: always fails")
			}
			okSeen++
			marks = append(marks, time.Since(start))
			done := okSeen >= healthy
			mu.Unlock()
			if done {
				once.Do(func() { close(allOK) })
			}

			return nil
		})

	client := newTestClient(t)
	receiver := rabbitmq.NewReceiver(client,
		append([]rabbitmq.ReceiverOption{rabbitmq.WithIncomingQueue(consumer)}, extra...)...)

	subCtx, stopSub := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- sub.Subscribe(subCtx, receiver) }()

	var elapsed time.Duration
	select {
	case <-allOK:
		mu.Lock()
		elapsed = time.Since(start)
		mu.Unlock()
	case <-time.After(3 * time.Minute):
		elapsed = 3 * time.Minute
	}
	stopSub()
	<-done

	mu.Lock()
	defer mu.Unlock()
	t.Logf("ONE POISON MESSAGE [%s] (prefetch=3 default, x-delivery-limit=20): %d/%d healthy handled in %v "+
		"(bad redelivered %d times). Healthy-message completion marks: %v",
		tag, okSeen, healthy, elapsed.Round(time.Millisecond), badTries, roundAll(marks))
}

// TestBriefDependencyBlipRecovery measures the cost of a routine 2-second
// dependency outage: every handler call fails for the first 2s, then all succeed.
func TestBriefDependencyBlipRecovery(t *testing.T) {
	requireMeasure(t)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer cancel()

	service, consumer := holSetup(t, "blip", 200)

	const total = 100
	subjects := make([]string, total)
	for i := range subjects {
		subjects[i] = fmt.Sprintf("m%d", i)
	}
	holPublish(t, ctx, service, subjects)

	const blip = 2 * time.Second

	var (
		mu       sync.Mutex
		seen     = map[string]bool{}
		start    time.Time
		blipTo   time.Time
		attempts int
	)
	allOK := make(chan struct{})
	var once sync.Once

	sd := &eventbus.ServiceDesc{ServiceName: service, Events: []eventbus.EventDesc{{Name: testEventName}}}
	sub := eventbus.NewSubscriber(consumer)
	sub.RegisterHandler(sd, testEventName,
		func(_ context.Context, md *event.Metadata, _ func(any) error, _ eventbus.SubscriberInterceptor) error {
			mu.Lock()
			if start.IsZero() {
				start = time.Now()
				blipTo = start.Add(blip)
			}
			attempts++
			down := time.Now().Before(blipTo)
			if down {
				mu.Unlock()

				return errors.New("dependency down")
			}
			seen[md.Subject] = true
			n := len(seen)
			mu.Unlock()
			if n >= total {
				once.Do(func() { close(allOK) })
			}

			return nil
		})

	client := newTestClient(t)
	receiver := rabbitmq.NewReceiver(client, rabbitmq.WithIncomingQueue(consumer))

	subCtx, stopSub := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- sub.Subscribe(subCtx, receiver) }()

	var elapsed time.Duration
	select {
	case <-allOK:
		mu.Lock()
		elapsed = time.Since(start)
		mu.Unlock()
	case <-time.After(4 * time.Minute):
		elapsed = 4 * time.Minute
	}
	stopSub()
	<-done

	mu.Lock()
	defer mu.Unlock()
	t.Logf("2s DEPENDENCY BLIP (prefetch=3 default): %d/%d messages cleared %v after the first delivery "+
		"(blip lasted %v, %d handler invocations total)",
		len(seen), total, elapsed.Round(time.Millisecond), blip, attempts)
}

func roundAll(ds []time.Duration) []time.Duration {
	out := make([]time.Duration, len(ds))
	for i, d := range ds {
		out[i] = d.Round(10 * time.Millisecond)
	}

	return out
}
