package rabbitmq_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"

	"github.com/quarks-tech/protoevent-go/pkg/event"
	"github.com/quarks-tech/protoevent-go/pkg/eventbus"
	"github.com/quarks-tech/protoevent-go/pkg/transport/rabbitmq"
)

// TestRequeuePacingStallsTheWholeConsumer pins the cost the requeue backoff
// imposes on messages that are NOT the one being paced.
//
// The pacing sleep runs on the single goroutine that reads deliveries, so it does
// not merely hold one prefetch slot — it stops the consumer. The observable
// signature is that healthy messages behind a permanently-failing one come through
// in groups of exactly prefetch-1, one group per delay. This test asserts that
// signature and, in the same run, that removing the pacing removes the stall
// entirely: without the second half, a change that made the delay a no-op would
// still pass the first.
//
// Deliberately NOT gated behind OUTBOX_MEASURE: this is the regression guard for
// the defaults chosen in consume.DefaultPrefetchCount and
// defaultRequeueBackoffMax, so it has to run in the ordinary suite. A flat 300ms
// backoff keeps it to a couple of seconds.
func TestRequeuePacingStallsTheWholeConsumer(t *testing.T) {
	const (
		delay    = 300 * time.Millisecond
		prefetch = 3
		perDelay = prefetch - 1 // what actually gets through per pacing sleep
		healthy  = 6            // three full groups
	)

	paced := runPacingScenario(t, "pacedon", prefetch, delay,
		[]rabbitmq.ReceiverOption{rabbitmq.WithRequeueBackoff(delay, delay)}, healthy)
	unpaced := runPacingScenario(t, "pacedoff", prefetch, delay,
		[]rabbitmq.ReceiverOption{rabbitmq.WithRequeueBackoff(0, 0)}, healthy)

	// The last healthy message cannot arrive before the pacing sleeps that precede
	// it have elapsed. groups-1 is the conservative form: the first group is already
	// buffered when the first sleep starts.
	groups := healthy / perDelay
	wantAtLeast := time.Duration(groups-1) * delay
	if paced.elapsed < wantAtLeast {
		t.Errorf("paced: %d healthy messages cleared in %v, want >= %v.\n"+
			"The pacing sleep is supposed to block the whole consume goroutine, so %d messages "+
			"behind a permanently-failing one take about %d sleeps to get through. Clearing them "+
			"faster means the delay is no longer being served on that goroutine — recheck what "+
			"doAcknowledge does before Reject.",
			healthy, paced.elapsed, wantAtLeast, healthy, groups)
	}

	// Mutation half: with pacing off the SAME scenario must clear immediately. If
	// this ever starts taking a delay's worth of time, something other than the
	// backoff is stalling the consumer and the number above stops meaning what it says.
	if unpaced.elapsed >= delay {
		t.Errorf("unpaced: %d healthy messages cleared in %v, want < %v.\n"+
			"With WithRequeueBackoff(0,0) there is no sleep, so the stall measured in the paced "+
			"case must vanish. It did not, so the paced number is not attributable to the pacing.",
			healthy, unpaced.elapsed, delay)
	}

	// The signature itself: paced deliveries arrive in bursts separated by ~delay,
	// unpaced ones do not.
	if paced.gaps < groups-1 {
		t.Errorf("paced: saw %d inter-arrival gaps >= %v among %d messages, want >= %d — "+
			"the healthy messages should arrive in groups of prefetch-1 (%d), one group per delay",
			paced.gaps, delay*3/4, healthy, groups-1, perDelay)
	}

	t.Logf("paced=%v (gaps>=%v: %d)  unpaced=%v  ratio=%.0fx",
		paced.elapsed.Round(time.Millisecond), (delay * 3 / 4), paced.gaps,
		unpaced.elapsed.Round(time.Millisecond),
		float64(paced.elapsed)/float64(max(unpaced.elapsed, time.Millisecond)))
}

type pacingResult struct {
	elapsed time.Duration
	gaps    int // inter-arrival gaps of roughly a full delay
}

// runPacingScenario publishes one permanently-failing message followed by
// `healthy` good ones and reports how long the good ones took to clear.
func runPacingScenario(
	t *testing.T,
	tag string,
	prefetch int,
	delay time.Duration,
	opts []rabbitmq.ReceiverOption,
	healthy int,
) pacingResult {
	t.Helper()

	inst := requireBroker(t)
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()

	service := tag + ".v1"
	consumer := tag + "-consumer"

	// Quorum topology declared externally, with a delivery limit high enough that
	// the failing message stays in the queue for the whole run: the point is what it
	// costs while it is there, not what happens when the broker finally discards it.
	inst.DeclareExchange(t, service, "topic")
	inst.DeclareQueue(t, consumer, amqp.Table{
		"x-queue-type":     "quorum",
		"x-delivery-limit": int32(1000),
	})
	inst.BindQueue(t, consumer, testEventName, service)
	t.Cleanup(func() { inst.DeleteQueue(t, consumer) })

	client := newTestClient(t)
	sd := &eventbus.ServiceDesc{ServiceName: service, Events: []eventbus.EventDesc{{Name: testEventName}}}
	sender := rabbitmq.NewSender(client)
	if err := sender.Setup(ctx, sd); err != nil {
		t.Fatalf("%s: sender setup: %v", tag, err)
	}

	subjects := make([]string, 0, 1+healthy)
	subjects = append(subjects, "bad")
	for range healthy {
		subjects = append(subjects, "ok")
	}
	for _, subj := range subjects {
		md := testEvent(service)
		md.Subject = subj
		if err := sender.Send(ctx, md, []byte("p")); err != nil {
			t.Fatalf("%s: send %q: %v", tag, subj, err)
		}
	}

	var (
		mu      sync.Mutex
		okSeen  int
		start   time.Time
		arrival []time.Time
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
				mu.Unlock()

				// A transient fault, not an unprocessable event: this is the class
				// that gets requeued and therefore paced.
				return errors.New("downstream 503")
			}
			okSeen++
			arrival = append(arrival, time.Now())
			done := okSeen >= healthy
			mu.Unlock()
			if done {
				once.Do(func() { close(allOK) })
			}

			return nil
		})

	receiver := rabbitmq.NewReceiver(client,
		append([]rabbitmq.ReceiverOption{
			rabbitmq.WithIncomingQueue(consumer),
			rabbitmq.WithPrefetchCount(prefetch),
		}, opts...)...)

	subCtx, stopSub := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- sub.Subscribe(subCtx, receiver) }()

	select {
	case <-allOK:
	case <-time.After(45 * time.Second):
		stopSub()
		<-done
		mu.Lock()
		n := okSeen
		mu.Unlock()
		t.Fatalf("%s: only %d/%d healthy messages cleared in 45s", tag, n, healthy)
	}
	stopSub()
	<-done

	mu.Lock()
	defer mu.Unlock()

	res := pacingResult{elapsed: arrival[len(arrival)-1].Sub(start)}
	// A "gap" is an inter-arrival pause long enough to be a pacing sleep rather than
	// ordinary broker latency. Three quarters of the delay separates the two by an
	// order of magnitude on any real broker.
	threshold := delay * 3 / 4
	prev := start
	for _, at := range arrival {
		if at.Sub(prev) >= threshold {
			res.gaps++
		}
		prev = at
	}

	return res
}
